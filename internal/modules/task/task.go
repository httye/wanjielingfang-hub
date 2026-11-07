// Package task 负责任务提交、计费、状态与产物下载。
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mcai/hub/internal/core"
)

const ddl = `
CREATE TABLE IF NOT EXISTS tasks (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id           INTEGER NOT NULL,
	site_id           INTEGER NOT NULL DEFAULT 0,
	kind              TEXT NOT NULL DEFAULT 'generate',
	platform          TEXT NOT NULL DEFAULT '',
	mc_version        TEXT NOT NULL DEFAULT '',
	status            TEXT NOT NULL DEFAULT 'pending',
	cost              INTEGER NOT NULL DEFAULT 0,
	priority          INTEGER NOT NULL DEFAULT 50,
	src               TEXT NOT NULL DEFAULT '',
	payload_json      TEXT NOT NULL DEFAULT '',
	worker_id         INTEGER NOT NULL DEFAULT 0,
	lease_until       INTEGER NOT NULL DEFAULT 0,
	attempts          INTEGER NOT NULL DEFAULT 0,
	fail_stage        TEXT NOT NULL DEFAULT '',
	error             TEXT NOT NULL DEFAULT '',
	jar_path          TEXT NOT NULL DEFAULT '',
	log_path          TEXT NOT NULL DEFAULT '',
	pass_at_1         INTEGER NOT NULL DEFAULT 0,
	fix_rounds        INTEGER NOT NULL DEFAULT 0,
	ai_tokens         INTEGER NOT NULL DEFAULT 0,
	ai_cost           INTEGER NOT NULL DEFAULT 0,
	iterations        INTEGER NOT NULL DEFAULT 0,
	parent_id         INTEGER NOT NULL DEFAULT 0,
	idem_key          TEXT NOT NULL DEFAULT '',
	refunded          INTEGER NOT NULL DEFAULT 0,
	created_at        INTEGER NOT NULL,
	started_at        INTEGER NOT NULL DEFAULT 0,
	finished_at       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tasks_queue ON tasks(status, priority DESC, id);
CREATE INDEX IF NOT EXISTS idx_tasks_user ON tasks(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_site ON tasks(site_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_lease ON tasks(status, lease_until);
`

// Module 是任务模块。
type Module struct {
	DataDir string
}

// New 构造任务模块（DataDir 存 jar 与日志）。
func New(dataDir string) *Module { return &Module{DataDir: dataDir} }

// Name 模块名。
func (m *Module) Name() string { return "task" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 本模块不额外声明配置。
func (m *Module) Defaults() map[string]map[string]any { return nil }

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("POST /api/v1/tasks", d.Auth.RequireSite(m.create(d)))
	mux.HandleFunc("GET /api/v1/tasks", d.Auth.RequireUser(m.list(d)))
	mux.HandleFunc("GET /api/v1/tasks/{id}", d.Auth.RequireUser(m.get(d)))
	mux.HandleFunc("GET /api/v1/tasks/{id}/download", m.download(d))
	mux.HandleFunc("POST /api/v1/tasks/{id}/iterate", d.Auth.RequireUser(m.iterate(d)))
	mux.HandleFunc("POST /api/v1/tasks/{id}/retry", d.Auth.RequireUser(m.retry(d)))
}

// ---------------------------------------------------------------------------
// 创建
// ---------------------------------------------------------------------------

// CreateReq 是提交任务的入参。
type CreateReq struct {
	UserID    int64
	SiteID    int64
	Kind      string
	Platform  string
	MCVersion string
	Src       string
	Payload   map[string]any
	IdemKey   string
	Priority  int64
}

// Create 提交任务：幂等 → 限额 → 计价 → 事务内建任务 + 扣费 + 发事件。
func Create(ctx context.Context, d *core.Deps, req CreateReq) (int64, int64, error) {
	if req.Kind == "" {
		req.Kind = "generate"
	}
	switch req.Kind {
	case "generate", "iterate", "adapt":
	default:
		return 0, 0, core.Errf("bad_kind", "不支持的任务类型: %s", req.Kind)
	}
	now := d.Unix()

	if req.IdemKey != "" {
		if id, ok := core.IdemFind(d.DB, req.IdemKey); ok {
			var cost int64
			_ = d.DB.QueryRow(`SELECT cost FROM tasks WHERE id = ?`, id).Scan(&cost)
			return id, cost, nil
		}
	}

	maxSrc := d.Cfg.Int("limits", "max_src_bytes", 262144)
	if int64(len(req.Src)) > maxSrc {
		return 0, 0, core.Errf("src_too_large", "源码超过 %d 字节上限", maxSrc)
	}

	rateLimit := d.Cfg.Int("billing", "rate_limit_per_min", 10)
	if ok, _, _ := core.Allow(d.DB, "task:"+itoa(req.UserID), rateLimit, 60, now); !ok {
		return 0, 0, core.NewErr(http.StatusTooManyRequests, "rate_limited", "提交过于频繁，请稍后再试")
	}
	maxActive := d.Cfg.Int("limits", "max_active_tasks", 2)
	if maxActive > 0 {
		var active int64
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE user_id = ? AND status IN ('pending','running')`, req.UserID).Scan(&active)
		if active >= maxActive {
			return 0, 0, core.NewErr(http.StatusTooManyRequests, "too_many_active",
				"同时进行的任务过多，请等待当前任务完成")
		}
	}

	price := d.Price.For(req.Kind, req.SiteID, req.Platform, now)
	priority := req.Priority
	if priority == 0 {
		priority = 50
	}
	payloadJSON := ""
	if len(req.Payload) > 0 {
		b, _ := json.Marshal(req.Payload)
		payloadJSON = string(b)
	}

	var taskID, cost int64
	err := core.Tx(ctx, d.DB, func(tx *sql.Tx) error {
		if req.IdemKey != "" {
			var existing int64
			if err := tx.QueryRow(`SELECT task_id FROM task_idem WHERE key = ?`, req.IdemKey).Scan(&existing); err == nil {
				taskID = existing
				// 命中幂等：返回库里真实的扣费金额，而不是本次新算的价。
				return tx.QueryRow(`SELECT cost FROM tasks WHERE id = ?`, existing).Scan(&cost)
			}
		}
		res, err := tx.Exec(`INSERT INTO tasks(user_id, site_id, kind, platform, mc_version, status, cost, priority,
			src, payload_json, idem_key, created_at)
			VALUES(?,?,?,?,?,'pending',?,?,?,?,?,?)`,
			req.UserID, req.SiteID, req.Kind, req.Platform, req.MCVersion, price, priority,
			req.Src, payloadJSON, req.IdemKey, now)
		if err != nil {
			return err
		}
		taskID, _ = res.LastInsertId()
		cost = price
		if price > 0 {
			if _, err := core.AddBalance(tx, req.UserID, -price, "task_"+req.Kind, "task", taskID, req.SiteID, now); err != nil {
				return err
			}
		}
		if req.IdemKey != "" {
			if err := core.IdemInsert(tx, req.IdemKey, taskID, now); err != nil {
				return err
			}
		}
		return d.Ev.Emit(tx, req.SiteID, "task.created", map[string]any{
			"task_id": taskID, "user_id": req.UserID, "kind": req.Kind,
			"platform": req.Platform, "mc_version": req.MCVersion, "cost": price,
		}, now)
	})
	if err != nil {
		return 0, 0, err
	}
	return taskID, cost, nil
}

func (m *Module) create(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Platform  string         `json:"platform"`
			MCVersion string         `json:"mc_version"`
			Src       string         `json:"src"`
			Kind      string         `json:"kind"`
			Payload   map[string]any `json:"payload"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		u, err := d.Auth.MustResolve(r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		s := core.SiteFrom(r)
		idem := r.Header.Get("Idempotency-Key")
		if idem == "" {
			idem = r.Header.Get("X-Idempotency-Key")
		}

		id, cost, err := Create(r.Context(), d, CreateReq{
			UserID:    u.ID,
			SiteID:    s.ID,
			Kind:      req.Kind,
			Platform:  req.Platform,
			MCVersion: req.MCVersion,
			Src:       req.Src,
			Payload:   req.Payload,
			IdemKey:   idem,
		})
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, map[string]any{"task_id": id, "cost": cost, "status": "pending"})
	}
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func (m *Module) list(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		p := core.NewPage(r)
		cursor := p.Cursor
		if cursor <= 0 {
			cursor = 1 << 62
		}
		status := r.URL.Query().Get("status")
		q := `SELECT id, site_id, kind, platform, mc_version, status, cost, attempts, fail_stage,
			pass_at_1, fix_rounds, created_at, finished_at FROM tasks
			WHERE user_id = ? AND id < ?`
		args := []any{u.ID, cursor}
		if status != "" {
			q += ` AND status = ?`
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
		var next int64
		for rows.Next() {
			var (
				id, siteID, cost, attempts, passAt1, fixRounds, created, finished int64
				kind, platform, version, st, failStage                            string
			)
			if rows.Scan(&id, &siteID, &kind, &platform, &version, &st, &cost, &attempts, &failStage,
				&passAt1, &fixRounds, &created, &finished) != nil {
				continue
			}
			next = id
			items = append(items, map[string]any{
				"id": id, "site_id": siteID, "kind": kind, "platform": platform, "mc_version": version,
				"status": st, "cost": cost, "attempts": attempts, "fail_stage": failStage,
				"pass_at_1": passAt1, "fix_rounds": fixRounds,
				"created_at": created, "finished_at": finished,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
	}
}

func (m *Module) get(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		var (
			taskID, userID, siteID, cost, priority, workerID, leaseUntil, attempts int64
			aiTokens, aiCost, iterations, parentID                                 int64
			created, started, finished                                             int64
			kind, platform, version, status, failStage, errMsg                     string
			passAt1, fixRounds, refunded                                           int64
		)
		err = d.DB.QueryRow(`SELECT id, user_id, site_id, kind, platform, mc_version, status, cost, priority,
			worker_id, lease_until, attempts, fail_stage, error, pass_at_1, fix_rounds, ai_tokens, ai_cost,
			iterations, parent_id, refunded, created_at, started_at, finished_at
			FROM tasks WHERE id = ?`, id).
			Scan(&taskID, &userID, &siteID, &kind, &platform, &version, &status, &cost, &priority,
				&workerID, &leaseUntil, &attempts, &failStage, &errMsg, &passAt1, &fixRounds, &aiTokens, &aiCost,
				&iterations, &parentID, &refunded, &created, &started, &finished)
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
			return
		}
		if userID != u.ID && !core.IsAdmin(u) {
			core.Fail(w, core.NewErr(http.StatusForbidden, "forbidden", "无权访问该任务"))
			return
		}

		var queuePos int64
		if status == "pending" {
			_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE status = 'pending'
				AND (priority > ? OR (priority = ? AND id < ?))`, priority, priority, taskID).Scan(&queuePos)
			queuePos++
		}

		out := map[string]any{
			"id": taskID, "site_id": siteID, "kind": kind, "platform": platform, "mc_version": version,
			"status": status, "cost": cost, "priority": priority, "attempts": attempts,
			"worker_id": workerID, "lease_until": leaseUntil,
			"fail_stage": failStage, "error": errMsg, "pass_at_1": passAt1, "fix_rounds": fixRounds,
			"ai_tokens": aiTokens, "ai_cost": aiCost, "iterations": iterations, "parent_id": parentID,
			"refunded": refunded == 1, "queue_position": queuePos,
			"created_at": created, "started_at": started, "finished_at": finished,
		}
		if status == "done" {
			out["download_url"] = d.Auth.SignedDownload(taskID, 10*time.Minute)
		}
		core.WriteJSON(w, http.StatusOK, out)
	}
}

// ---------------------------------------------------------------------------
// 下载
// ---------------------------------------------------------------------------

func (m *Module) download(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		// 1) 签名优先（卫星站/前端拿短期链接直接下载）
		sig := r.URL.Query().Get("sig")
		exp := core.QueryInt(r, "exp", 0)
		signedOK := false
		if sig != "" && exp > 0 {
			signedOK = d.Auth.VerifySign(r.URL.Path, exp, sig)
		}
		if !signedOK {
			u, uerr := d.Auth.MustResolve(r)
			if uerr != nil {
				core.Fail(w, uerr)
				return
			}
			var owner int64
			if err := d.DB.QueryRow(`SELECT user_id FROM tasks WHERE id = ?`, id).Scan(&owner); err != nil {
				core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
				return
			}
			if owner != u.ID && !core.IsAdmin(u) {
				core.Fail(w, core.NewErr(http.StatusForbidden, "forbidden", "无权下载该产物"))
				return
			}
		}

		var (
			status  string
			jarPath string
		)
		if err := d.DB.QueryRow(`SELECT status, jar_path FROM tasks WHERE id = ?`, id).Scan(&status, &jarPath); err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
			return
		}
		if status != "done" || jarPath == "" {
			core.Fail(w, core.NewErr(http.StatusConflict, "not_ready", "产物尚未生成"))
			return
		}
		base := filepath.Base(jarPath)
		if base != jarPath || base == "." || base == ".." {
			core.Fail(w, core.NewErr(http.StatusForbidden, "bad_path", "非法产物路径"))
			return
		}
		abs := filepath.Join(m.DataDir, base)
		f, err := os.Open(abs)
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "artifact_missing", "产物文件缺失"))
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/java-archive")
		w.Header().Set("Content-Disposition", `attachment; filename="plugin-`+itoa(id)+`.jar"`)
		var modTime time.Time
		if st, serr := os.Stat(abs); serr == nil {
			modTime = st.ModTime()
		}
		http.ServeContent(w, r, filepath.Base(abs), modTime, f)
	}
}

// ---------------------------------------------------------------------------
// 迭代 / 重试
// ---------------------------------------------------------------------------

func (m *Module) iterate(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		var req struct {
			Requirement string `json:"requirement"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if strings.TrimSpace(req.Requirement) == "" {
			core.Fail(w, core.Errf("bad_requirement", "请描述要修改的内容"))
			return
		}
		var (
			owner, siteID, iterations int64
			platform, version, src    string
			status                    string
		)
		err = d.DB.QueryRow(`SELECT user_id, site_id, platform, mc_version, src, iterations, status
			FROM tasks WHERE id = ?`, id).
			Scan(&owner, &siteID, &platform, &version, &src, &iterations, &status)
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
			return
		}
		if owner != u.ID && !core.IsAdmin(u) {
			core.Fail(w, core.NewErr(http.StatusForbidden, "forbidden", "无权迭代该任务"))
			return
		}
		if status == "running" {
			core.Fail(w, core.NewErr(http.StatusConflict, "task_running", "任务正在构建中，请等待完成后再迭代"))
			return
		}
		newID, cost, err := Create(r.Context(), d, CreateReq{
			UserID:    owner,
			SiteID:    siteID,
			Kind:      "iterate",
			Platform:  platform,
			MCVersion: version,
			Src:       src,
			Payload:   map[string]any{"requirement": req.Requirement, "parent_id": id, "iterations": iterations + 1},
			IdemKey:   r.Header.Get("Idempotency-Key"),
		})
		if err != nil {
			core.Fail(w, err)
			return
		}
		_, _ = d.DB.Exec(`UPDATE tasks SET parent_id = ?, iterations = ? WHERE id = ?`, id, iterations+1, newID)
		core.WriteJSON(w, http.StatusCreated, map[string]any{"task_id": newID, "cost": cost, "kind": "iterate"})
	}
}

func (m *Module) retry(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		var (
			owner, siteID, refunded int64
			platform, version, src  string
			status                  string
		)
		err = d.DB.QueryRow(`SELECT user_id, site_id, platform, mc_version, src, status, refunded
			FROM tasks WHERE id = ?`, id).
			Scan(&owner, &siteID, &platform, &version, &src, &status, &refunded)
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
			return
		}
		if owner != u.ID && !core.IsAdmin(u) {
			core.Fail(w, core.NewErr(http.StatusForbidden, "forbidden", "无权操作该任务"))
			return
		}
		if status != "failed" {
			core.Fail(w, core.NewErr(http.StatusConflict, "not_failed", "只有失败的任务可以重试"))
			return
		}
		if refunded != 1 {
			core.Fail(w, core.NewErr(http.StatusConflict, "not_refunded", "该任务不满足免费重试条件"))
			return
		}
		now := d.Unix()
		res, err := d.DB.Exec(`INSERT INTO tasks(user_id, site_id, kind, platform, mc_version, status, cost, priority,
			src, payload_json, parent_id, created_at)
			SELECT user_id, site_id, kind, platform, mc_version, 'pending', 0, priority,
				src, payload_json, id, ? FROM tasks WHERE id = ?`, now, id)
		if err != nil {
			core.Fail(w, err)
			return
		}
		newID, _ := res.LastInsertId()
		_, _ = d.DB.Exec(`UPDATE tasks SET refunded = 2 WHERE id = ?`, id)
		_ = d.Ev.EmitDirect(siteID, "task.created", map[string]any{
			"task_id": newID, "user_id": owner, "kind": "retry", "parent_id": id, "cost": 0,
		}, now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{"task_id": newID, "cost": 0, "retry_of": id})
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
