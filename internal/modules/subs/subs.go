// Package subs 负责插件维护订阅：按月扣费、新版本自动生成适配任务。
package subs

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mcai/hub/internal/core"
	"github.com/mcai/hub/internal/modules/task"
)

const ddl = `
CREATE TABLE IF NOT EXISTS subscriptions (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id         INTEGER NOT NULL,
	task_id         INTEGER NOT NULL,
	status          TEXT NOT NULL DEFAULT 'active',
	price           INTEGER NOT NULL DEFAULT 0,
	next_bill_at    INTEGER NOT NULL,
	target_versions TEXT NOT NULL DEFAULT '',
	paused_reason   TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_subs_due ON subscriptions(status, next_bill_at);
CREATE INDEX IF NOT EXISTS idx_subs_user ON subscriptions(user_id, id DESC);
`

const monthSeconds = 30 * 86400

// Module 是订阅模块。
type Module struct{}

// New 构造。
func New() *Module { return &Module{} }

// Name 模块名。
func (m *Module) Name() string { return "subs" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 无额外配置（价格走 billing）。
func (m *Module) Defaults() map[string]map[string]any { return nil }

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("POST /api/v1/subscriptions", d.Auth.RequireUser(m.create(d)))
	mux.HandleFunc("GET /api/v1/subscriptions", d.Auth.RequireUser(m.list(d)))
	mux.HandleFunc("POST /api/v1/subscriptions/{id}/cancel", d.Auth.RequireUser(m.cancel(d)))
	mux.HandleFunc("POST /api/v1/subscriptions/{id}/resume", d.Auth.RequireUser(m.resume(d)))
}

func (m *Module) create(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TaskID         int64    `json:"task_id"`
			TargetVersions []string `json:"target_versions"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		u := core.UserFrom(r)
		var owner int64
		var status string
		if err := d.DB.QueryRow(`SELECT user_id, status FROM tasks WHERE id = ?`, req.TaskID).Scan(&owner, &status); err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
			return
		}
		if owner != u.ID && !core.IsAdmin(u) {
			core.Fail(w, core.NewErr(http.StatusForbidden, "forbidden", "只能订阅自己的任务"))
			return
		}
		if status != "done" {
			core.Fail(w, core.NewErr(http.StatusConflict, "not_done", "任务完成后才能订阅"))
			return
		}
		if !d.Cfg.Bool("features", "subscription", true) {
			core.Fail(w, core.NewErr(http.StatusForbidden, "disabled", "订阅功能未开放"))
			return
		}
		var dup int64
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM subscriptions
			WHERE user_id = ? AND task_id = ? AND status = 'active'`, u.ID, req.TaskID).Scan(&dup)
		if dup > 0 {
			core.Fail(w, core.NewErr(http.StatusConflict, "already_subscribed", "该任务已订阅"))
			return
		}
		now := d.Unix()
		price := d.Cfg.Int("billing", "task_cost", 10) * 30
		res, err := d.DB.Exec(`INSERT INTO subscriptions(user_id, task_id, status, price, next_bill_at, target_versions, created_at, updated_at)
			VALUES(?,?,'active',?,?,?,?,?)`,
			u.ID, req.TaskID, price, now+monthSeconds, strings.Join(req.TargetVersions, ","), now, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "price": price, "next_bill_at": now + monthSeconds})
	}
}

func (m *Module) list(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		rows, err := d.DB.Query(`SELECT id, task_id, status, price, next_bill_at, target_versions, paused_reason, created_at
			FROM subscriptions WHERE user_id = ? ORDER BY id DESC`, u.ID)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, taskID, price, nextBill, created int64
				status, versions, reason             string
			)
			if rows.Scan(&id, &taskID, &status, &price, &nextBill, &versions, &reason, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "task_id": taskID, "status": status, "price": price,
				"next_bill_at": nextBill, "target_versions": versions, "paused_reason": reason, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) own(d *core.Deps, r *http.Request) (int64, error) {
	u := core.UserFrom(r)
	id, err := core.PathInt(r, "id")
	if err != nil {
		return 0, err
	}
	var owner int64
	if err := d.DB.QueryRow(`SELECT user_id FROM subscriptions WHERE id = ?`, id).Scan(&owner); err != nil {
		return 0, core.NewErr(http.StatusNotFound, "not_found", "订阅不存在")
	}
	if owner != u.ID && !core.IsAdmin(u) {
		return 0, core.NewErr(http.StatusForbidden, "forbidden", "无权操作该订阅")
	}
	return id, nil
}

func (m *Module) cancel(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.own(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		now := d.Unix()
		if _, err := d.DB.Exec(`UPDATE subscriptions SET status = 'cancelled', updated_at = ? WHERE id = ?`, now, id); err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "cancelled"})
	}
}

func (m *Module) resume(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.own(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		now := d.Unix()
		if _, err := d.DB.Exec(`UPDATE subscriptions SET status = 'active', paused_reason = '', next_bill_at = ?, updated_at = ?
			WHERE id = ?`, now+monthSeconds, now, id); err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "active"})
	}
}

// ---------------------------------------------------------------------------
// 计费循环
// ---------------------------------------------------------------------------

// BillingLoop 每 5 分钟检查到期订阅；余额不足置 paused 并记录原因。
// 间隔可用 BILLING_INTERVAL（秒）覆盖，便于测试与运维调优。
func BillingLoop(ctx context.Context, d *core.Deps) {
	interval := 5 * time.Minute
	if v := d.EnvOr("BILLING_INTERVAL", ""); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			interval = time.Duration(sec) * time.Second
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			billOnce(ctx, d)
		}
	}
}

func billOnce(ctx context.Context, d *core.Deps) {
	now := d.Unix()
	rows, err := d.DB.QueryContext(ctx, `SELECT id, user_id, price FROM subscriptions
		WHERE status = 'active' AND next_bill_at <= ? LIMIT 100`, now)
	if err != nil {
		return
	}
	type item struct{ id, userID, price int64 }
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.userID, &it.price) == nil {
			items = append(items, it)
		}
	}
	rows.Close()

	for _, it := range items {
		err := core.Tx(ctx, d.DB, func(tx *sql.Tx) error {
			if it.price > 0 {
				if _, err := core.AddBalance(tx, it.userID, -it.price, "subscription", "subscription", it.id, 0, now); err != nil {
					return err
				}
			}
			_, err := tx.Exec(`UPDATE subscriptions SET next_bill_at = ?, updated_at = ? WHERE id = ?`,
				now+monthSeconds, now, it.id)
			return err
		})
		if err != nil {
			reason := "余额不足，已暂停"
			if err != core.ErrInsufficient {
				reason = "扣费失败，已暂停"
			}
			_, _ = d.DB.Exec(`UPDATE subscriptions SET status = 'paused', paused_reason = ?, updated_at = ? WHERE id = ?`,
				reason, now, it.id)
			d.Log.Printf("subscription %d paused: %v", it.id, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 新版本适配
// ---------------------------------------------------------------------------

// SpawnAdaptTasks 在 mc_versions 新增版本时，给每个活跃订阅生成适配任务。
// 用 IdemKey 保证同一订阅 + 同一版本只生成一次（版本删掉再加回不会重复扣费）。
func SpawnAdaptTasks(ctx context.Context, d *core.Deps, added []string) int {
	if len(added) == 0 {
		return 0
	}
	rows, err := d.DB.QueryContext(ctx, `SELECT id, user_id, task_id FROM subscriptions WHERE status = 'active'`)
	if err != nil {
		return 0
	}
	type sub struct{ id, userID, taskID int64 }
	var list []sub
	for rows.Next() {
		var s sub
		if rows.Scan(&s.id, &s.userID, &s.taskID) == nil {
			list = append(list, s)
		}
	}
	rows.Close()

	created := 0
	for _, s := range list {
		var platform, src string
		if err := d.DB.QueryRow(`SELECT platform, src FROM tasks WHERE id = ?`, s.taskID).Scan(&platform, &src); err != nil {
			continue
		}
		for _, v := range added {
			_, _, err := task.Create(ctx, d, task.CreateReq{
				UserID:    s.userID,
				SiteID:    0,
				Kind:      "adapt",
				Platform:  platform,
				MCVersion: v,
				Src:       src,
				IdemKey:   "adapt:" + itoa(s.id) + ":" + v,
				Payload:   map[string]any{"reason": "new_mc_version", "subscription_id": s.id, "parent_task": s.taskID},
				Priority:  30,
			})
			if err != nil {
				d.Log.Printf("adapt task for sub %d/%s: %v", s.id, v, err)
				continue
			}
			created++
		}
	}
	return created
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
