package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Config 是 hub 的配置中心：分区存储、版本号单调递增、可合并站点级覆盖。
// 卫星站只拿到合并后的结果，永远不接触密钥类字段。
type Config struct {
	DB  *sql.DB
	TTL time.Duration

	mu       sync.RWMutex
	cache    map[string]map[string]any
	version  int64
	cachedAt time.Time

	// setMu 串行化版本号分配，避免并发 Set 取到同一个 version。
	setMu sync.Mutex
}

// NewConfig 构造配置中心。
func NewConfig(db *sql.DB) *Config {
	return &Config{DB: db, TTL: 2 * time.Second}
}

// Init 写入各模块声明的默认值（已存在的分区不动）。
func (c *Config) Init(defaults map[string]map[string]any) error {
	now := time.Now().Unix()
	for section, val := range defaults {
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		if _, err := c.DB.Exec(`INSERT INTO config(section, value, version, updated_at)
			VALUES(?,?,1,?) ON CONFLICT(section) DO NOTHING`, section, string(b), now); err != nil {
			return err
		}
	}
	c.invalidate()
	return nil
}

// Snapshot 返回当前版本与全部分区（带短 TTL 缓存）。
func (c *Config) Snapshot() (int64, map[string]map[string]any, error) {
	now := time.Now()
	c.mu.RLock()
	if c.cache != nil && now.Sub(c.cachedAt) < c.TTL {
		v, data := c.version, c.cache
		c.mu.RUnlock()
		return v, data, nil
	}
	c.mu.RUnlock()

	rows, err := c.DB.Query(`SELECT section, value, version FROM config`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	out := map[string]map[string]any{}
	var maxv int64
	for rows.Next() {
		var section, raw string
		var ver int64
		if err := rows.Scan(&section, &raw, &ver); err != nil {
			return 0, nil, err
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			obj = map[string]any{"value": raw}
		}
		out[section] = obj
		if ver > maxv {
			maxv = ver
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	c.mu.Lock()
	c.cache, c.version, c.cachedAt = out, maxv, now
	c.mu.Unlock()
	return maxv, out, nil
}

// Version 返回当前版本号。
func (c *Config) Version() int64 {
	v, _, err := c.Snapshot()
	if err != nil {
		return 0
	}
	return v
}

// Section 取一个分区并拷贝一份，避免调用方改动缓存。
func (c *Config) Section(name string) map[string]any {
	_, all, err := c.Snapshot()
	if err != nil {
		return map[string]any{}
	}
	src := all[name]
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Int 读分区里的整数配置，缺失时用默认值。
func (c *Config) Int(section, key string, def int64) int64 {
	m := c.Section(section)
	v, ok := m[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return def
}

// Str 读分区里的字符串配置。
func (c *Config) Str(section, key, def string) string {
	m := c.Section(section)
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// Bool 读分区里的布尔配置。
func (c *Config) Bool(section, key string, def bool) bool {
	m := c.Section(section)
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// Set 全量替换一个分区，并让版本号 +1。
func (c *Config) Set(section string, value any, now int64) (int64, error) {
	c.setMu.Lock()
	defer c.setMu.Unlock()

	b, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	var v int64
	if err := c.DB.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM config`).Scan(&v); err != nil {
		return 0, err
	}
	if _, err := c.DB.Exec(`INSERT INTO config(section, value, version, updated_at)
		VALUES(?,?,?,?)
		ON CONFLICT(section) DO UPDATE SET value=excluded.value, version=excluded.version, updated_at=excluded.updated_at`,
		section, string(b), v, now); err != nil {
		return 0, err
	}
	c.invalidate()
	return v, nil
}

// Patch 在分区内合并若干键（admin 改单项配置走这里）。
func (c *Config) Patch(section string, patch map[string]any, now int64) (int64, error) {
	cur := c.Section(section)
	for k, v := range patch {
		cur[k] = v
	}
	return c.Set(section, cur, now)
}

// Delete 删除分区（谨慎使用，一般只用于下线功能）。
func (c *Config) Delete(section string) error {
	if _, err := c.DB.Exec(`DELETE FROM config WHERE section = ?`, section); err != nil {
		return err
	}
	c.invalidate()
	return nil
}

func (c *Config) invalidate() {
	c.mu.Lock()
	c.cache = nil
	c.cachedAt = time.Time{}
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 站点级覆盖
// ---------------------------------------------------------------------------

// MergeSiteConfig 把站点 override 合并到全局配置上（section → 键覆盖）。
// override 的 JSON 形状与全局配置一致：{"billing":{"task_cost":5},...}
func MergeSiteConfig(base map[string]map[string]any, overrideJSON string) map[string]map[string]any {
	out := make(map[string]map[string]any, len(base))
	for section, kv := range base {
		cp := make(map[string]any, len(kv))
		for k, v := range kv {
			cp[k] = v
		}
		out[section] = cp
	}
	if overrideJSON == "" {
		return out
	}
	var ov map[string]map[string]any
	if err := json.Unmarshal([]byte(overrideJSON), &ov); err != nil {
		return out
	}
	for section, kv := range ov {
		dst, ok := out[section]
		if !ok {
			dst = map[string]any{}
			out[section] = dst
		}
		for k, v := range kv {
			dst[k] = v
		}
	}
	return out
}

// ErrConfigMissing 表示配置分区不存在。
var ErrConfigMissing = errors.New("config section missing")
