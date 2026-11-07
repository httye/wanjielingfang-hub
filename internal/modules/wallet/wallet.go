// Package wallet 负责余额流水、CDK、USDT/支付宝支付单与套餐。
package wallet

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/mcai/hub/internal/core"
)

const ddl = `
CREATE TABLE IF NOT EXISTS cdks (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	code       TEXT NOT NULL UNIQUE,
	amount     INTEGER NOT NULL,
	batch      TEXT NOT NULL DEFAULT '',
	used_by    INTEGER NOT NULL DEFAULT 0,
	used_at    INTEGER NOT NULL DEFAULT 0,
	created_by INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS payments (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id      INTEGER NOT NULL,
	channel      TEXT NOT NULL,
	amount_cents INTEGER NOT NULL DEFAULT 0,
	points       INTEGER NOT NULL DEFAULT 0,
	txid         TEXT NOT NULL DEFAULT '',
	chain        TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT 'pending',
	note         TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	confirmed_at INTEGER NOT NULL DEFAULT 0,
	confirmed_by INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pay_txid ON payments(chain, txid) WHERE txid <> '';
CREATE INDEX IF NOT EXISTS idx_pay_user ON payments(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_pay_status ON payments(status, id);
`

// Module 是钱包模块。
type Module struct{}

// New 构造。
func New() *Module { return &Module{} }

// Name 模块名。
func (m *Module) Name() string { return "wallet" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 声明计费与功能开关分区。
func (m *Module) Defaults() map[string]map[string]any {
	return map[string]map[string]any{
		"billing": {
			"task_cost":          10,
			"iterate_cost":       4,
			"credit_cost_fail":   1,
			"signup_bonus":       20,
			"referral_bonus":     10,
			"commission_rate":    10,
			"usdt_rate":          10,
			"rate_limit_per_min": 10,
			"max_active_tasks":   2,
			"refund_on_fail":     false,
			"min_task_cost":      1,
			"plans": []any{
				map[string]any{"id": "monthly", "name": "月卡", "price_cents": 30000, "points": 1500, "days": 30},
				map[string]any{"id": "quarterly", "name": "季卡", "price_cents": 80000, "points": 5000, "days": 90},
			},
			"channels": map[string]any{"cdk": true, "usdt": true, "alipay": false},
		},
	}
}

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("GET /api/config", m.publicConfig(d))
	mux.HandleFunc("GET /api/plans", m.plans(d))
	mux.HandleFunc("POST /api/redeem", d.Auth.RequireUser(m.redeem(d)))
	mux.HandleFunc("GET /api/txns", d.Auth.RequireUser(m.txns(d)))
	mux.HandleFunc("GET /api/payments", d.Auth.RequireUser(m.myPayments(d)))
	mux.HandleFunc("POST /api/payments/usdt", d.Auth.RequireUser(m.usdt(d)))
	mux.HandleFunc("POST /api/payments/alipay", d.Auth.RequireUser(m.alipay(d)))
	mux.HandleFunc("POST /api/alipay/notify", m.alipayNotify(d))
	mux.HandleFunc("POST /api/plans/buy", d.Auth.RequireUser(m.buyPlan(d)))
}

// ---------------------------------------------------------------------------
// 公开配置
// ---------------------------------------------------------------------------

func (m *Module) publicConfig(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ver, _, err := d.Cfg.Snapshot()
		if err != nil {
			core.Fail(w, err)
			return
		}
		out := map[string]any{
			"config_version": ver,
			"brand":          d.Cfg.Section("brand"),
			"billing":        d.Cfg.Section("billing"),
			"catalog":        d.Cfg.Section("catalog"),
			"features":       d.Cfg.Section("features"),
			"limits":         d.Cfg.Section("limits"),
			"auth": map[string]any{
				"register_open":   d.Cfg.Bool("auth", "register_open", true),
				"invite_required": d.Cfg.Bool("auth", "invite_required", false),
				"hosted_login":    d.Cfg.Str("auth", "hosted_login", "/auth/login"),
				"hosted_register": d.Cfg.Str("auth", "hosted_register", "/auth/register"),
			},
		}
		core.WriteJSON(w, http.StatusOK, out)
	}
}

func (m *Module) plans(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		core.WriteJSON(w, http.StatusOK, map[string]any{"plans": d.Cfg.Section("billing")["plans"]})
	}
}

// ---------------------------------------------------------------------------
// CDK
// ---------------------------------------------------------------------------

func (m *Module) redeem(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code string `json:"code"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		code := strings.ToUpper(strings.TrimSpace(req.Code))
		if code == "" {
			core.Fail(w, core.Errf("bad_code", "请输入兑换码"))
			return
		}
		u := core.UserFrom(r)
		now := d.Unix()
		if ok, _, _ := core.Allow(d.DB, "redeem:"+u.Email, 10, 300, now); !ok {
			core.Fail(w, core.NewErr(http.StatusTooManyRequests, "rate_limited", "尝试过于频繁"))
			return
		}

		var (
			amount int64
			newBal int64
		)
		err := core.Tx(r.Context(), d.DB, func(tx *sql.Tx) error {
			var id, usedBy int64
			if err := tx.QueryRow(`SELECT id, amount, used_by FROM cdks WHERE code = ?`, code).
				Scan(&id, &amount, &usedBy); err != nil {
				return core.NewErr(http.StatusNotFound, "cdk_not_found", "兑换码不存在")
			}
			if usedBy != 0 {
				return core.NewErr(http.StatusConflict, "cdk_used", "兑换码已被使用")
			}
			res, err := tx.Exec(`UPDATE cdks SET used_by = ?, used_at = ? WHERE id = ? AND used_by = 0`, u.ID, now, id)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return core.NewErr(http.StatusConflict, "cdk_used", "兑换码已被使用")
			}
			var berr error
			newBal, berr = core.AddBalance(tx, u.ID, amount, "cdk", "cdk", id, 0, now)
			return berr
		})
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.Audit(d.DB, u.ID, "cdk_redeem", code, "", core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "amount": amount, "balance": newBal})
	}
}

// ---------------------------------------------------------------------------
// 流水
// ---------------------------------------------------------------------------

func (m *Module) txns(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		p := core.NewPage(r)
		rows, err := d.DB.Query(`SELECT id, delta, balance_after, reason, ref_type, ref_id, site_id, created_at
			FROM txns WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT ?`,
			u.ID, orMax(p.Cursor), p.Limit)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		var next int64
		for rows.Next() {
			var (
				id, delta, after, refID, siteID, at int64
				reason, refType                     string
			)
			if rows.Scan(&id, &delta, &after, &reason, &refType, &refID, &siteID, &at) != nil {
				continue
			}
			next = id
			items = append(items, map[string]any{
				"id": id, "delta": delta, "balance_after": after, "reason": reason,
				"ref_type": refType, "ref_id": refID, "site_id": siteID, "created_at": at,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
	}
}

func orMax(v int64) int64 {
	if v <= 0 {
		return 1 << 62
	}
	return v
}

// ---------------------------------------------------------------------------
// 支付单
// ---------------------------------------------------------------------------

func (m *Module) myPayments(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		rows, err := d.DB.Query(`SELECT id, channel, amount_cents, points, txid, chain, status, note, created_at
			FROM payments WHERE user_id = ? ORDER BY id DESC LIMIT 50`, u.ID)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, cents, points, at              int64
				channel, txid, chain, status, note string
			)
			if rows.Scan(&id, &channel, &cents, &points, &txid, &chain, &status, &note, &at) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "channel": channel, "amount_cents": cents, "points": points,
				"txid": txid, "chain": chain, "status": status, "note": note, "created_at": at,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

var allowedChains = map[string]bool{"TRC20": true, "ERC20": true, "BEP20": true, "POLYGON": true, "OTHER": true}

func (m *Module) usdt(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Txid        string `json:"txid"`
			Chain       string `json:"chain"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		req.Txid = strings.TrimSpace(req.Txid)
		req.Chain = strings.ToUpper(strings.TrimSpace(req.Chain))
		if req.Chain == "" {
			req.Chain = "TRC20"
		}
		if !allowedChains[req.Chain] {
			core.Fail(w, core.Errf("bad_chain", "不支持的链"))
			return
		}
		if len(req.Txid) < 16 || len(req.Txid) > 128 {
			core.Fail(w, core.Errf("bad_txid", "txid 格式不正确"))
			return
		}
		if req.AmountCents <= 0 {
			core.Fail(w, core.Errf("bad_amount", "请填写支付金额"))
			return
		}
		u := core.UserFrom(r)
		now := d.Unix()
		rate := d.Cfg.Int("billing", "usdt_rate", 10) // 1 USDT = N 积分
		points := req.AmountCents * rate / 100
		if points <= 0 {
			core.Fail(w, core.Errf("bad_amount", "金额过小"))
			return
		}
		res, err := d.DB.Exec(`INSERT INTO payments(user_id, channel, amount_cents, points, txid, chain, status, note, created_at)
			VALUES(?, 'usdt', ?, ?, ?, ?, 'pending', '', ?)`,
			u.ID, req.AmountCents, points, req.Txid, req.Chain, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				core.Fail(w, core.NewErr(http.StatusConflict, "duplicate_txid", "该 txid 已提交过"))
				return
			}
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, u.ID, "usdt_submit", req.Txid, req.Chain, core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{
			"id": id, "status": "pending", "points": points, "chain": req.Chain,
		})
	}
}

func (m *Module) alipay(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		core.WriteJSON(w, http.StatusNotImplemented, map[string]any{
			"error":   "not_configured",
			"message": "支付宝渠道尚未配置 ALIPAY_APP_ID，二期接入",
		})
	}
}

func (m *Module) alipayNotify(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 占位：二期接入 SDK 后在此验签并调用 CreditPayment。
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte("not_implemented"))
	}
}

// ---------------------------------------------------------------------------
// 套餐
// ---------------------------------------------------------------------------

func (m *Module) buyPlan(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PlanID string `json:"plan_id"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		price, points, _, ok := findPlan(d, req.PlanID)
		if !ok {
			core.Fail(w, core.NewErr(http.StatusNotFound, "plan_not_found", "套餐不存在"))
			return
		}
		u := core.UserFrom(r)
		now := d.Unix()

		// 套餐只能走支付渠道购买（CDK/USDT/支付宝），不允许用积分余额直接抵扣：
		// 积分是服务额度，不是货币；否则「花 300 元得 1500 积分」与「用 300 积分买套餐」两套口径会互相打架。
		// 下单后进入 pending，admin（或支付回调）确认时统一由 CreditPayment 加积分 + 续会员天数。
		res, err := d.DB.Exec(`INSERT INTO payments(user_id, channel, amount_cents, points, txid, chain, status, note, created_at)
			VALUES(?, 'plan', ?, ?, '', '', 'pending', ?, ?)`, u.ID, price, points, req.PlanID, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.WriteJSON(w, http.StatusCreated, map[string]any{
			"id": id, "status": "pending", "plan": req.PlanID,
			"amount_cents": price, "points": points,
			"pay_hint": "支付完成后由后台确认入账（或接入支付回调）",
		})
	}
}

func applyPlanDays(tx *sql.Tx, d *core.Deps, userID int64, planID string, now int64) error {
	_, _, days, ok := findPlan(d, planID)
	if !ok || days <= 0 {
		return nil
	}
	_, err := tx.Exec(`UPDATE users SET vip_until = MAX(vip_until, ?) + ?, updated_at = ? WHERE id = ?`,
		now, days*86400, now, userID)
	return err
}

func findPlan(d *core.Deps, id string) (priceCents, points, days int64, ok bool) {
	arr, _ := d.Cfg.Section("billing")["plans"].([]any)
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := m["id"].(string); s == id {
			return toI(m["price_cents"]), toI(m["points"]), toI(m["days"]), true
		}
	}
	return 0, 0, 0, false
}

func toI(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// ---------------------------------------------------------------------------
// 支付入账（admin 与回调共用）
// ---------------------------------------------------------------------------

// CreditPayment 把一张 pending 支付单标记为已确认并入账，返回新余额。
func CreditPayment(ctx context.Context, d *core.Deps, payID, actorID int64) (int64, error) {
	now := d.Unix()
	var newBal int64
	err := core.Tx(ctx, d.DB, func(tx *sql.Tx) error {
		var (
			userID  int64
			points  int64
			channel string
			status  string
			note    string
		)
		if err := tx.QueryRow(`SELECT user_id, points, channel, status, note FROM payments WHERE id = ?`, payID).
			Scan(&userID, &points, &channel, &status, &note); err != nil {
			return core.NewErr(http.StatusNotFound, "not_found", "支付单不存在")
		}
		if status != "pending" {
			return core.NewErr(http.StatusConflict, "already_confirmed", "该支付单已处理")
		}
		res, err := tx.Exec(`UPDATE payments SET status = 'confirmed', confirmed_at = ?, confirmed_by = ?
			WHERE id = ? AND status = 'pending'`, now, actorID, payID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return core.NewErr(http.StatusConflict, "already_confirmed", "该支付单已处理")
		}
		var berr error
		newBal, berr = core.AddBalance(tx, userID, points, channel, "payment", payID, 0, now)
		if berr != nil {
			return berr
		}
		if channel == "plan" && note != "" {
			if err := applyPlanDays(tx, d, userID, note, now); err != nil {
				return err
			}
		}
		return nil
	})
	return newBal, err
}
