// Package admin 提供运营后台接口：统计、CDK、支付确认、审计、配置热更新。
package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mcai/hub/internal/core"
	"github.com/mcai/hub/internal/modules/subs"
	"github.com/mcai/hub/internal/modules/wallet"
)

// Module 是管理后台模块。
type Module struct{}

// New 构造。
func New() *Module { return &Module{} }

// Name 模块名。
func (m *Module) Name() string { return "admin" }

// Migrate 无自有表。
func (m *Module) Migrate(db *sql.DB) error { return nil }

// Defaults 无配置。
func (m *Module) Defaults() map[string]map[string]any { return nil }

// Routes 挂载路由（全部要求 admin）。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	a := d.Auth.RequireAdmin
	mux.HandleFunc("GET /api/admin/dashboard", a(m.dashboard(d)))
	mux.HandleFunc("GET /api/admin/stats", a(m.stats(d)))
	mux.HandleFunc("GET /api/admin/config", a(m.getConfig(d)))
	mux.HandleFunc("POST /api/admin/config", a(m.setConfig(d)))
	mux.HandleFunc("POST /api/admin/cdks", a(m.genCDKs(d)))
	mux.HandleFunc("GET /api/admin/cdks", a(m.listCDKs(d)))
	mux.HandleFunc("GET /api/admin/payments", a(m.listPayments(d)))
	mux.HandleFunc("POST /api/admin/payments/{id}/confirm", a(m.confirmPayment(d)))
	mux.HandleFunc("GET /api/admin/audit", a(m.audit(d)))
	mux.HandleFunc("GET /api/admin/users", a(m.users(d)))
	mux.HandleFunc("GET /api/admin/sites", a(m.sites(d)))
	mux.HandleFunc("POST /api/admin/sites", a(m.createSite(d)))
	mux.HandleFunc("POST /api/admin/price-rules", a(m.priceRule(d)))
	mux.HandleFunc("POST /api/admin/price-rules/{id}", a(m.updatePriceRule(d)))
	mux.HandleFunc("POST /api/admin/price-rules/{id}/delete", a(m.deletePriceRule(d)))
	mux.HandleFunc("GET /api/admin/price-rules", a(m.listPriceRules(d)))
	mux.HandleFunc("GET /api/admin/tasks", a(m.tasks(d)))
	mux.HandleFunc("GET /api/admin/workers", a(m.workers(d)))
	mux.HandleFunc("POST /api/admin/workers", a(m.createWorker(d)))
	mux.HandleFunc("POST /api/admin/workers/{id}/rotate", a(m.rotateWorker(d)))
	mux.HandleFunc("POST /api/admin/workers/{id}/delete", a(m.deleteWorker(d)))
}

// createSite 由管理员为指定用户开通一个卫星站，返回一次性凭据。
func (m *Module) createSite(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID     int64  `json:"user_id"`
			Name       string `json:"name"`
			BaseURL    string `json:"base_url"`
			WebhookURL string `json:"webhook_url"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.UserID <= 0 {
			core.Fail(w, core.Errf("bad_user", "请填写归属用户 ID"))
			return
		}
		var exists int
		if err := d.DB.QueryRow(`SELECT COUNT(1) FROM users WHERE id = ?`, req.UserID).Scan(&exists); err != nil || exists == 0 {
			core.Fail(w, core.Errf("bad_user", "用户不存在"))
			return
		}
		if req.Name == "" || req.BaseURL == "" {
			core.Fail(w, core.Errf("bad_site", "站名与站点地址必填"))
			return
		}
		key := core.SlugKey()
		secret := core.RandomToken(24)
		now := d.Unix()
		res, err := d.DB.Exec(`INSERT INTO sites(user_id, name, site_key, base_url, webhook_url, webhook_secret,
			api_version, site_version, config_override, revoked, created_at)
			VALUES(?,?,?,?,?,?,1,'','',0,?)`,
			req.UserID, req.Name, key, strings.TrimRight(req.BaseURL, "/"), req.WebhookURL, secret, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, core.UserFrom(r).ID, "site_create_admin", req.Name, fmt.Sprint(req.UserID), core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{
			"id": id, "user_id": req.UserID, "name": req.Name,
			"site_key": key, "webhook_secret": secret,
		})
	}
}

// stats 返回看板用的时间序列与分布数据。
func (m *Module) stats(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days := core.QueryInt(r, "days", 7)
		if days <= 0 || days > 90 {
			days = 7
		}
		now := d.Unix()
		since := now - days*86400

		type point struct {
			Day    string `json:"day"`
			Tasks  int64  `json:"tasks"`
			Done   int64  `json:"done"`
			Failed int64  `json:"failed"`
			Users  int64  `json:"users"`
			Points int64  `json:"points"`
			Tokens int64  `json:"tokens"`
		}
		idx := map[string]*point{}
		series := make([]*point, 0, days)
		for i := days - 1; i >= 0; i-- {
			t := time.Unix(now-int64(i)*86400, 0).UTC()
			key := t.Format("2006-01-02")
			p := &point{Day: key}
			idx[key] = p
			series = append(series, p)
		}

		fill := func(q string, set func(p *point, a, b int64)) {
			rows, err := d.DB.Query(q, since)
			if err != nil {
				return
			}
			defer rows.Close()
			for rows.Next() {
				var day string
				var a, b int64
				if rows.Scan(&day, &a, &b) == nil {
					if p, ok := idx[day]; ok {
						set(p, a, b)
					}
				}
			}
		}
		fill(`SELECT date(created_at,'unixepoch'), COUNT(1), SUM(CASE WHEN status='done' THEN 1 ELSE 0 END)
			FROM tasks WHERE created_at >= ? GROUP BY 1`, func(p *point, a, b int64) { p.Tasks, p.Done = a, b })
		fill(`SELECT date(created_at,'unixepoch'), SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END), 0
			FROM tasks WHERE created_at >= ? GROUP BY 1`, func(p *point, a, _ int64) { p.Failed = a })
		fill(`SELECT date(created_at,'unixepoch'), COUNT(1), 0 FROM users WHERE created_at >= ? GROUP BY 1`,
			func(p *point, a, _ int64) { p.Users = a })
		fill(`SELECT date(created_at,'unixepoch'), SUM(CASE WHEN delta < 0 THEN -delta ELSE 0 END), 0
			FROM txns WHERE created_at >= ? GROUP BY 1`, func(p *point, a, _ int64) { p.Points = a })
		fill(`SELECT date(created_at,'unixepoch'), SUM(total_tokens), 0 FROM ai_usage WHERE created_at >= ? GROUP BY 1`,
			func(p *point, a, _ int64) { p.Tokens = a })

		dist := func(q, key string) []map[string]any {
			out := []map[string]any{}
			rows, err := d.DB.Query(q)
			if err != nil {
				return out
			}
			defer rows.Close()
			for rows.Next() {
				var name string
				var n int64
				if rows.Scan(&name, &n) == nil {
					out = append(out, map[string]any{key: name, "count": n})
				}
			}
			return out
		}

		core.WriteJSON(w, http.StatusOK, map[string]any{
			"days":        days,
			"series":      series,
			"by_status":   dist(`SELECT status, COUNT(1) FROM tasks GROUP BY status ORDER BY 2 DESC`, "status"),
			"by_platform": dist(`SELECT COALESCE(NULLIF(platform,''),'未指定'), COUNT(1) FROM tasks GROUP BY 1 ORDER BY 2 DESC LIMIT 8`, "platform"),
			"by_site": dist(`SELECT COALESCE(s.name,'无站点'), COUNT(1) FROM tasks t
				LEFT JOIN sites s ON s.id = t.site_id GROUP BY 1 ORDER BY 2 DESC LIMIT 8`, "name"),
		})
	}
}

func (m *Module) dashboard(d *core.Deps) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		now := d.Unix()
		var users, tasks, running, done, failed, revenue, pendingPay, workers, online, queue int64
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM users`).Scan(&users)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks`).Scan(&tasks)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE status = 'running'`).Scan(&running)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE status = 'done'`).Scan(&done)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE status = 'failed'`).Scan(&failed)
		_ = d.DB.QueryRow(`SELECT COALESCE(SUM(delta),0) FROM txns WHERE delta > 0 AND reason NOT LIKE 'task_%'`).Scan(&revenue)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM payments WHERE status = 'pending'`).Scan(&pendingPay)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM workers`).Scan(&workers)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM workers WHERE last_heartbeat > 0 AND last_heartbeat > ?`, now-120).Scan(&online)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE status = 'pending'`).Scan(&queue)

		successRate := 0.0
		if done+failed > 0 {
			successRate = float64(done) * 100 / float64(done+failed)
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{
			"users": users, "tasks": tasks,
			"queue": queue, "running": running, "done": done, "failed": failed,
			"success_rate":     successRate,
			"points_credited":  revenue,
			"pending_payments": pendingPay,
			"workers":          workers, "workers_online": online,
			"config_version": d.Cfg.Version(),
			"server_time":    now,
		})
	}
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

func (m *Module) getConfig(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ver, all, err := d.Cfg.Snapshot()
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"config_version": ver, "sections": all})
	}
}

func (m *Module) setConfig(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Section string         `json:"section"`
			Values  map[string]any `json:"values"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.Section == "" || len(req.Values) == 0 {
			core.Fail(w, core.Errf("bad_request", "section 与 values 必填"))
			return
		}

		before := d.Cfg.Section(req.Section)
		addedVersions := []string{}
		if req.Section == "catalog" {
			if nv, ok := req.Values["mc_versions"]; ok {
				addedVersions = diffStrings(toStrings(before["mc_versions"]), toStrings(nv))
			}
		}

		ver, err := d.Cfg.Patch(req.Section, req.Values, d.Unix())
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "config_set", req.Section, jsonish(req.Values), core.ClientIP(r), d.Unix())

		spawned := 0
		if len(addedVersions) > 0 {
			spawned = subs.SpawnAdaptTasks(r.Context(), d, addedVersions)
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{
			"ok": true, "config_version": ver, "added_versions": addedVersions, "adapt_tasks": spawned,
		})
	}
}

func jsonish(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func diffStrings(oldList, newList []string) []string {
	seen := map[string]bool{}
	for _, s := range oldList {
		seen[s] = true
	}
	var added []string
	for _, s := range newList {
		if !seen[s] {
			added = append(added, s)
		}
	}
	return added
}

// ---------------------------------------------------------------------------
// CDK
// ---------------------------------------------------------------------------

func (m *Module) genCDKs(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count  int64  `json:"count"`
			Amount int64  `json:"amount"`
			Batch  string `json:"batch"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.Count <= 0 || req.Count > 500 {
			core.Fail(w, core.Errf("bad_count", "数量需在 1-500 之间"))
			return
		}
		if req.Amount <= 0 {
			core.Fail(w, core.Errf("bad_amount", "面额需大于 0"))
			return
		}
		if req.Batch == "" {
			req.Batch = "default"
		}
		u := core.UserFrom(r)
		now := d.Unix()
		codes := make([]string, 0, req.Count)
		err := core.Tx(r.Context(), d.DB, func(tx *sql.Tx) error {
			for i := int64(0); i < req.Count; i++ {
				code := "MC-" + core.RandomCode(4) + "-" + core.RandomCode(4)
				if _, err := tx.Exec(`INSERT INTO cdks(code, amount, batch, created_by, created_at) VALUES(?,?,?,?,?)`,
					code, req.Amount, req.Batch, u.ID, now); err != nil {
					return err
				}
				codes = append(codes, code)
			}
			return nil
		})
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.Audit(d.DB, u.ID, "cdk_generate", req.Batch, "", core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{"batch": req.Batch, "amount": req.Amount, "codes": codes})
	}
}

func (m *Module) listCDKs(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(`SELECT id, code, amount, batch, used_by, used_at, created_at
			FROM cdks ORDER BY id DESC LIMIT 200`)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id, amount, usedBy, usedAt, created int64
			var code, batch string
			if rows.Scan(&id, &code, &amount, &batch, &usedBy, &usedAt, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "code": code, "amount": amount, "batch": batch,
				"used_by": usedBy, "used_at": usedAt, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// ---------------------------------------------------------------------------
// 支付确认
// ---------------------------------------------------------------------------

func (m *Module) listPayments(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		q := `SELECT id, user_id, channel, amount_cents, points, txid, chain, status, note, created_at FROM payments`
		args := []any{}
		if status != "" {
			q += ` WHERE status = ?`
			args = append(args, status)
		}
		q += ` ORDER BY id DESC LIMIT 200`
		rows, err := d.DB.Query(q, args...)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, userID, cents, points, created int64
				channel, txid, chain, st, note     string
			)
			if rows.Scan(&id, &userID, &channel, &cents, &points, &txid, &chain, &st, &note, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "user_id": userID, "channel": channel, "amount_cents": cents, "points": points,
				"txid": txid, "chain": chain, "status": st, "note": note, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) confirmPayment(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		u := core.UserFrom(r)
		bal, err := wallet.CreditPayment(r.Context(), d, id, u.ID)
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.Audit(d.DB, u.ID, "payment_confirm", itoa(id), "", core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "payment_id": id, "user_balance": bal})
	}
}

// ---------------------------------------------------------------------------
// 审计 / 列表
// ---------------------------------------------------------------------------

func (m *Module) audit(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := core.NewPage(r)
		cursor := p.Cursor
		if cursor <= 0 {
			cursor = 1 << 62
		}
		rows, err := d.DB.Query(`SELECT id, actor_id, action, target, detail, ip, created_at
			FROM audit_logs WHERE id < ? ORDER BY id DESC LIMIT ?`, cursor, p.Limit)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		var next int64
		for rows.Next() {
			var (
				id, actor, created         int64
				action, target, detail, ip string
			)
			if rows.Scan(&id, &actor, &action, &target, &detail, &ip, &created) != nil {
				continue
			}
			next = id
			items = append(items, map[string]any{
				"id": id, "actor_id": actor, "action": action, "target": target,
				"detail": detail, "ip": ip, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
	}
}

func (m *Module) users(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(`SELECT id, email, username, role, balance, signup_granted, vip_until, created_at
			FROM users ORDER BY id DESC LIMIT 200`)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, bal, granted, vip, created int64
				email, username, role          string
			)
			if rows.Scan(&id, &email, &username, &role, &bal, &granted, &vip, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "email": email, "username": username, "role": role, "balance": bal,
				"signup_granted": granted == 1, "vip_until": vip, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) sites(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(`SELECT s.id, s.name, s.user_id, s.base_url, s.api_version, s.site_version,
			s.ai_tokens, s.ai_cost, s.revoked, s.created_at,
			(SELECT COUNT(1) FROM tasks t WHERE t.site_id = s.id) AS tasks
			FROM sites s ORDER BY s.id DESC LIMIT 200`)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, userID, apiVer, tokens, cost, created, tasks int64
				name, base, siteVer                              string
				revoked                                          int64
			)
			if rows.Scan(&id, &name, &userID, &base, &apiVer, &siteVer, &tokens, &cost, &revoked, &created, &tasks) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "name": name, "user_id": userID, "base_url": base,
				"api_version": apiVer, "site_version": siteVer, "ai_tokens": tokens, "ai_cost": cost,
				"revoked": revoked == 1, "tasks": tasks, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) workers(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(`SELECT id, name, capabilities, status, last_heartbeat, created_at
			FROM workers ORDER BY id DESC LIMIT 200`)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		now := d.Unix()
		items := []map[string]any{}
		for rows.Next() {
			var id, last, created int64
			var name, caps, status string
			if rows.Scan(&id, &name, &caps, &status, &last, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "name": name, "capabilities": caps, "status": status,
				"last_heartbeat": last, "online": last > 0 && last > now-120, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// createWorker 由管理员手动登记一台构建节点，返回一次性 worker_key。
func (m *Module) createWorker(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name         string   `json:"name"`
			Capabilities []string `json:"capabilities"`
			Note         string   `json:"note"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			core.Fail(w, core.Errf("bad_name", "请填写节点名称"))
			return
		}
		// 能力为空表示通吃所有平台
		caps := make([]string, 0, len(req.Capabilities))
		for _, c := range req.Capabilities {
			c = strings.TrimSpace(c)
			if c != "" {
				caps = append(caps, c)
			}
		}
		key := core.RandomToken(24)
		now := d.Unix()
		// last_heartbeat=0 表示「从未连接」：手动登记不等于节点已上线。
		res, err := d.DB.Exec(`INSERT INTO workers(name, worker_key, capabilities, status, last_heartbeat, created_at)
			VALUES(?,?,?,'idle',0,?)`, req.Name, key, strings.Join(caps, "|"), now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, core.UserFrom(r).ID, "worker_create_admin", req.Name, req.Note, core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{
			"id": id, "name": req.Name, "capabilities": strings.Join(caps, "|"), "worker_key": key,
		})
	}
}

// rotateWorker 重置节点密钥：旧 key 立即失效，节点需要用新 key 重启。
func (m *Module) rotateWorker(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		key := core.RandomToken(24)
		res, err := d.DB.Exec(`UPDATE workers SET worker_key = ? WHERE id = ?`, key, id)
		if err != nil {
			core.Fail(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "节点不存在"))
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "worker_rotate", fmt.Sprint(id), "", core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "worker_key": key})
	}
}

// deleteWorker 删除节点；正在跑任务时拒绝，避免任务卡在 running。
func (m *Module) deleteWorker(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		var running int64
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE worker_id = ? AND status = 'running'`, id).Scan(&running)
		if running > 0 {
			core.Fail(w, core.NewErr(http.StatusConflict, "worker_busy",
				fmt.Sprintf("该节点还有 %d 个任务在跑，先等它完成或租约超时", running)))
			return
		}
		res, err := d.DB.Exec(`DELETE FROM workers WHERE id = ?`, id)
		if err != nil {
			core.Fail(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "节点不存在"))
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "worker_delete", "", fmt.Sprint(id), core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (m *Module) priceRule(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action    string `json:"action"`
			SiteID    int64  `json:"site_id"`
			Platform  string `json:"platform"`
			Amount    int64  `json:"amount"`
			Currency  string `json:"currency"`
			ValidFrom int64  `json:"valid_from"`
			ValidTo   int64  `json:"valid_to"`
			Priority  int64  `json:"priority"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.Action == "" {
			core.Fail(w, core.Errf("bad_action", "action 必填"))
			return
		}
		if req.Currency == "" {
			req.Currency = "CNY"
		}
		res, err := d.DB.Exec(`INSERT INTO price_rules(action, site_id, platform, amount, currency, valid_from, valid_to, priority)
			VALUES(?,?,?,?,?,?,?,?)`, req.Action, req.SiteID, req.Platform, req.Amount, req.Currency,
			req.ValidFrom, req.ValidTo, req.Priority)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, core.UserFrom(r).ID, "price_rule", req.Action, "", core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusCreated, map[string]any{"id": id})
	}
}

func (m *Module) updatePriceRule(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		var req struct {
			Action    string `json:"action"`
			SiteID    int64  `json:"site_id"`
			Platform  string `json:"platform"`
			Amount    int64  `json:"amount"`
			Currency  string `json:"currency"`
			ValidFrom int64  `json:"valid_from"`
			ValidTo   int64  `json:"valid_to"`
			Priority  int64  `json:"priority"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.Action == "" {
			core.Fail(w, core.Errf("bad_action", "action 必填"))
			return
		}
		if req.Currency == "" {
			req.Currency = "CNY"
		}
		res, err := d.DB.Exec(`UPDATE price_rules SET action = ?, site_id = ?, platform = ?, amount = ?,
			currency = ?, valid_from = ?, valid_to = ?, priority = ? WHERE id = ?`,
			req.Action, req.SiteID, req.Platform, req.Amount, req.Currency,
			req.ValidFrom, req.ValidTo, req.Priority, id)
		if err != nil {
			core.Fail(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "规则不存在"))
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "price_rule_update", req.Action, fmt.Sprint(id), core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	}
}

func (m *Module) deletePriceRule(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		res, err := d.DB.Exec(`DELETE FROM price_rules WHERE id = ?`, id)
		if err != nil {
			core.Fail(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "规则不存在"))
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "price_rule_delete", "", fmt.Sprint(id), core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (m *Module) listPriceRules(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(`SELECT id, action, site_id, platform, amount, currency,
			valid_from, valid_to, priority FROM price_rules ORDER BY priority DESC, id DESC LIMIT 200`)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, siteID, amount, from, to, prio int64
				action, platform, currency         string
			)
			if rows.Scan(&id, &action, &siteID, &platform, &amount, &currency, &from, &to, &prio) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "action": action, "site_id": siteID, "platform": platform,
				"amount": amount, "currency": currency, "valid_from": from, "valid_to": to, "priority": prio,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) tasks(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		p := core.NewPage(r)
		q := `SELECT id, user_id, site_id, kind, platform, mc_version, status, cost, attempts,
			pass_at_1, fix_rounds, fail_stage, created_at FROM tasks`
		args := []any{}
		if status != "" {
			q += ` WHERE status = ?`
			args = append(args, status)
		}
		q += ` ORDER BY id DESC LIMIT ?`
		args = append(args, p.Limit)
		rows, err := d.DB.Query(q, args...)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, userID, siteID, cost, attempts, passAt1, fixRounds, created int64
				kind, platform, version, st, failStage                          string
			)
			if rows.Scan(&id, &userID, &siteID, &kind, &platform, &version, &st, &cost, &attempts,
				&passAt1, &fixRounds, &failStage, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "user_id": userID, "site_id": siteID, "kind": kind,
				"platform": platform, "mc_version": version, "status": st, "cost": cost,
				"attempts": attempts, "pass_at_1": passAt1, "fix_rounds": fixRounds,
				"fail_stage": failStage, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
