// Package core 提供 hub 的基础设施：数据库、迁移、模块装配、依赖注入。
package core

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// 数据库
// ---------------------------------------------------------------------------

// Open 打开 SQLite（modernc 纯 Go 实现），并写入 WAL / busy_timeout 等必需 pragma。
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir data dir: %w", err)
		}
	}
	// 下面这些 pragma 都是 per-connection 的，必须由 DSN 承载：
	// 只在新开的某条连接上 Exec，池里其它连接拿不到设置，并发写会直接撞 SQLITE_BUSY。
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	dsn := path + sep + "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite 写串行；4 个连接足够让读不排队，写靠 busy_timeout 等待。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	// 兜底：万一 DSN 参数被驱动忽略，至少让第一条连接带上设置（失败不阻断启动）。
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
	} {
		_ = execPragma(db, p)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}

func execPragma(db *sql.DB, stmt string) error {
	rows, err := db.Query(stmt)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// Tx 执行一个事务，自动回滚。
func Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// 迁移
// ---------------------------------------------------------------------------

// ApplyMigration 幂等地执行一个模块的 DDL。DDL 内部以分号分割（不允许语句里带分号）。
func ApplyMigration(db *sql.DB, name, version, ddl string) error {
	tag := name + ":" + version
	var n int
	err := db.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, tag).Scan(&n)
	if err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			return err
		}
		n = 0
	}
	if n > 0 {
		return nil
	}
	for _, stmt := range strings.Split(ddl, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate %s: %w\n--- stmt ---\n%s", tag, err, stmt)
		}
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(?, ?)`, tag, time.Now().Unix())
	return err
}

// InitSchema 建迁移记录表本身。
func InitSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`)
	return err
}

// ---------------------------------------------------------------------------
// 模块
// ---------------------------------------------------------------------------

// Module 是 hub 的可扩展单元：自带路由、自带建表、自带默认配置。
// 新增业务（货架 / 工单 / 企业版…）只需要实现该接口并在 main 里注册一行。
type Module interface {
	Name() string
	Migrate(db *sql.DB) error
	Defaults() map[string]map[string]any
	Routes(mux *http.ServeMux, d *Deps)
}

// Registry 保存已注册模块。
type Registry struct {
	mu   sync.Mutex
	mods []Module
}

func (r *Registry) Register(m ...Module) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mods = append(r.mods, m...)
}

func (r *Registry) All() []Module {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Module, len(r.mods))
	copy(out, r.mods)
	return out
}

// ---------------------------------------------------------------------------
// 依赖
// ---------------------------------------------------------------------------

// AIOptions 上游模型配置，只存在于 hub 进程内，永不下发卫星站。
type AIOptions struct {
	BaseURL string
	APIKey  string
	Model   string
	Mock    bool
}

// Deps 是模块拿到的全部依赖。
type Deps struct {
	DB     *sql.DB
	Auth   *Auth
	Cfg    *Config
	Ev     *Events
	Price  *Price
	Log    *log.Logger
	Secret string
	AI     AIOptions
	HTTP   *http.Client
	Env    map[string]string

	// Now 可注入，便于测试。
	Now func() time.Time
}

// Unix 返回统一的时间戳（秒），全库时间都是 INTEGER unix 秒，不出现方言函数。
func (d *Deps) Unix() int64 {
	if d.Now != nil {
		return d.Now().Unix()
	}
	return time.Now().Unix()
}

// EnvOr 读取环境变量默认值。
func (d *Deps) EnvOr(key, def string) string {
	if v, ok := d.Env[key]; ok && v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// 通用小工具
// ---------------------------------------------------------------------------

// RandomToken 生成 n 字节熵的 url-safe 令牌。
func RandomToken(n int) string { return randToken(n) }

// RandomCode 生成 n 字节熵的大写 hex 短码（邀请码 / CDK 用）。
func RandomCode(n int) string { return randHex(n) }

// SlugKey 生成站点 key（sk_ 前缀）。
func SlugKey() string { return "sk_" + randToken(24) }
