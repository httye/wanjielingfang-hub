package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------------------
// 计价
// ---------------------------------------------------------------------------

// Price 按规则表算价，命中不到就回落到 config 里的默认值。
type Price struct {
	DB  *sql.DB
	Cfg *Config
}

// NewPrice 构造计价器。
func NewPrice(db *sql.DB, cfg *Config) *Price { return &Price{DB: db, Cfg: cfg} }

// For 返回某动作在某站点/平台上的价格（积分）。
// 优先级：price_rules 精确规则 → 站点自己的 billing 覆盖 → 中心站 billing 默认值。
func (p *Price) For(action string, siteID int64, platform string, now int64) int64 {
	var amount int64
	err := p.DB.QueryRow(`SELECT amount FROM price_rules
		WHERE action = ?
		  AND (site_id = 0 OR site_id = ?)
		  AND (platform = '' OR platform = ?)
		  AND (valid_from = 0 OR valid_from <= ?)
		  AND (valid_to = 0 OR valid_to >= ?)
		ORDER BY priority DESC, site_id DESC, id DESC LIMIT 1`,
		action, siteID, platform, now, now).Scan(&amount)
	if err == nil {
		return amount
	}
	// 站点级定价：站长可以在自己的站点里改价（受中心站下限约束）。
	if siteID > 0 {
		if v, ok := p.sitePrice(siteID, action); ok {
			return v
		}
	}
	// 回落：billing 分区里与 action 同名的键。
	def := map[string]int64{
		"generate":    p.Cfg.Int("billing", "task_cost", 10),
		"iterate":     p.Cfg.Int("billing", "iterate_cost", 4),
		"adapt":       p.Cfg.Int("billing", "iterate_cost", 4),
		"credit_fail": p.Cfg.Int("billing", "credit_cost_fail", 1),
		"signup":      p.Cfg.Int("billing", "signup_bonus", 20),
		"referral":    p.Cfg.Int("billing", "referral_bonus", 10),
	}
	if v, ok := def[action]; ok {
		return v
	}
	return p.Cfg.Int("billing", action, 0)
}

// sitePrice 从 sites.config_override 的 billing 分区取价。
func (p *Price) sitePrice(siteID int64, action string) (int64, bool) {
	key := map[string]string{
		"generate":    "task_cost",
		"iterate":     "iterate_cost",
		"adapt":       "iterate_cost",
		"credit_fail": "credit_cost_fail",
	}[action]
	if key == "" {
		return 0, false
	}
	var raw string
	if err := p.DB.QueryRow(`SELECT config_override FROM sites WHERE id = ? AND revoked = 0`, siteID).Scan(&raw); err != nil {
		return 0, false
	}
	if raw == "" {
		return 0, false
	}
	var ov map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &ov); err != nil {
		return 0, false
	}
	billing, ok := ov["billing"]
	if !ok {
		return 0, false
	}
	v, ok := billing[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 限流
// ---------------------------------------------------------------------------

// Allow 固定窗口限流。返回是否放行、当前窗口计数。
func Allow(db *sql.DB, key string, limit, windowSec, now int64) (bool, int64, error) {
	if limit <= 0 {
		return true, 0, nil
	}
	if windowSec <= 0 {
		windowSec = 60
	}
	win := now / windowSec * windowSec
	var count int64
	err := db.QueryRow(`INSERT INTO rate_counters(key, window_start, count) VALUES(?,?,1)
		ON CONFLICT(key) DO UPDATE SET
			count = CASE WHEN rate_counters.window_start = ? THEN rate_counters.count + 1 ELSE 1 END,
			window_start = ?
		RETURNING count`, key, win, win, win).Scan(&count)
	if err != nil {
		// 限流器故障时不阻塞业务。
		return true, 0, err
	}
	return count <= limit, count, nil
}

// ---------------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------------

// IdemFind 按幂等键查已有任务。
func IdemFind(db *sql.DB, key string) (int64, bool) {
	var id int64
	if err := db.QueryRow(`SELECT task_id FROM task_idem WHERE key = ?`, key).Scan(&id); err != nil {
		return 0, false
	}
	return id, true
}

// IdemInsert 在事务里登记幂等键。
func IdemInsert(tx *sql.Tx, key string, taskID, now int64) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO task_idem(key, task_id, created_at) VALUES(?,?,?)`, key, taskID, now)
	return err
}

// ---------------------------------------------------------------------------
// Webhook 签名
// ---------------------------------------------------------------------------

// WebhookSignature 计算回调签名：HMAC-SHA256(secret, "ts.body")。
func WebhookSignature(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhook 供卫星站侧复用（放在这里方便两侧共用同一套算法）。
func VerifyWebhook(secret string, ts int64, body []byte, sig string) bool {
	return hmac.Equal([]byte(WebhookSignature(secret, ts, body)), []byte(sig))
}

// ---------------------------------------------------------------------------
// 分页
// ---------------------------------------------------------------------------

// Page 统一分页参数。
type Page struct {
	Limit  int64 `json:"limit"`
	Cursor int64 `json:"cursor"`
	Next   int64 `json:"next"`
}

// NewPage 解析并收敛 limit。
func NewPage(r *http.Request) Page {
	limit := QueryInt(r, "limit", 20)
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return Page{Limit: limit, Cursor: QueryInt(r, "cursor", 0)}
}
