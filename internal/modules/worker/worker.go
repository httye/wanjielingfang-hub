// Package worker 负责构建节点的注册、认领、心跳与结果上报。
package worker

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mcai/hub/internal/core"
	"github.com/mcai/hub/internal/modules/task"
)

const ddl = `
CREATE TABLE IF NOT EXISTS workers (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	name           TEXT NOT NULL,
	worker_key     TEXT NOT NULL UNIQUE,
	capabilities   TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL DEFAULT 'idle',
	last_heartbeat INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workers_heartbeat ON workers(last_heartbeat DESC)
`

const leaseSeconds = 300

// Module 是 worker 模块。
type Module struct {
	DataDir   string
	EnrollKey string
}

// New 构造。
func New(dataDir, enrollKey string) *Module { return &Module{DataDir: dataDir, EnrollKey: enrollKey} }

// Name 模块名。
func (m *Module) Name() string { return "worker" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 本模块无配置。
func (m *Module) Defaults() map[string]map[string]any { return nil }

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("POST /api/v1/worker/register", m.register(d))
	mux.HandleFunc("POST /api/v1/worker/claim", d.Auth.RequireWorker(m.claim(d)))
	mux.HandleFunc("POST /api/v1/worker/tasks/{id}/result", d.Auth.RequireWorker(m.result(d)))
	mux.HandleFunc("POST /api/v1/worker/heartbeat", d.Auth.RequireWorker(m.heartbeat(d)))
	mux.HandleFunc("GET /api/v1/worker/me", d.Auth.RequireWorker(m.me(d)))
}

func (m *Module) register(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.EnrollKey != "" && r.Header.Get("X-Enroll-Key") != m.EnrollKey {
			core.Fail(w, core.NewErr(http.StatusUnauthorized, "bad_enroll_key", "注册密钥无效"))
			return
		}
		var req struct {
			Name         string `json:"name"`
			Capabilities string `json:"capabilities"` // "paper|spigot|fabric"
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			req.Name = "worker"
		}
		key := core.RandomToken(24)
		now := d.Unix()
		res, err := d.DB.Exec(`INSERT INTO workers(name, worker_key, capabilities, status, last_heartbeat, created_at)
			VALUES(?,?,?,'idle',?,?)`, req.Name, key, normalizeCaps(req.Capabilities), now, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, 0, "worker_register", req.Name, "", core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{"worker_id": id, "worker_key": key, "name": req.Name})
	}
}

func normalizeCaps(s string) string {
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "|")
}

// parseInt 解析表单里的整数字段，非法值按 0 处理。
func parseInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (m *Module) me(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := core.WorkerID(r)
		var (
			name, caps, status string
			last               int64
		)
		if err := d.DB.QueryRow(`SELECT name, capabilities, status, last_heartbeat FROM workers WHERE id = ?`, wid).
			Scan(&name, &caps, &status, &last); err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{
			"worker_id": wid, "name": name, "capabilities": caps, "status": status, "last_heartbeat": last,
		})
	}
}

// claim 原子认领：单条 UPDATE ... RETURNING 完成挑选与占用，避免两个 worker 抢同一任务。
func (m *Module) claim(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := core.WorkerID(r)
		now := d.Unix()
		var (
			caps      string
			taskID    int64
			userID    int64
			siteID    int64
			kind      string
			platform  string
			mcVersion string
			src       string
			payload   string
			cost      int64
			attempts  int64
		)
		_ = d.DB.QueryRow(`SELECT capabilities FROM workers WHERE id = ?`, wid).Scan(&caps)
		pattern := ""
		if caps != "" {
			pattern = "|" + caps + "|"
		}

		err := d.DB.QueryRow(`UPDATE tasks SET status = 'running', worker_id = ?, lease_until = ?,
			started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END, attempts = attempts + 1
			WHERE id = (
				SELECT id FROM tasks
				WHERE status = 'pending' AND attempts < 3
				  AND (? = '' OR platform = '' OR instr(?, '|' || platform || '|') > 0)
				ORDER BY priority DESC, created_at ASC LIMIT 1
			)
			RETURNING id, user_id, site_id, kind, platform, mc_version, src, payload_json, cost, attempts`,
			wid, now+leaseSeconds, now, pattern, pattern).
			Scan(&taskID, &userID, &siteID, &kind, &platform, &mcVersion, &src, &payload, &cost, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			core.Fail(w, err)
			return
		}
		_, _ = d.DB.Exec(`UPDATE workers SET status = 'busy', last_heartbeat = ? WHERE id = ?`, now, wid)

		core.WriteJSON(w, http.StatusOK, map[string]any{
			"task_id": taskID, "user_id": userID, "site_id": siteID, "kind": kind,
			"platform": platform, "mc_version": mcVersion, "src": src,
			"payload": payload, "cost": cost, "attempts": attempts,
			"lease_seconds": leaseSeconds,
		})
	}
}

func (m *Module) heartbeat(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := core.WorkerID(r)
		now := d.Unix()
		if _, err := d.DB.Exec(`UPDATE workers SET last_heartbeat = ?, status = CASE
			WHEN EXISTS(SELECT 1 FROM tasks WHERE worker_id = ? AND status = 'running') THEN 'busy' ELSE 'idle' END
			WHERE id = ?`, now, wid, wid); err != nil {
			core.Fail(w, err)
			return
		}
		// 续租：正在跑的任务继续持有租约。
		if _, err := d.DB.Exec(`UPDATE tasks SET lease_until = ? WHERE worker_id = ? AND status = 'running'`,
			now+leaseSeconds, wid); err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "server_time": now})
	}
}

func (m *Module) result(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := core.WorkerID(r)
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			core.Fail(w, core.NewErr(http.StatusBadRequest, "bad_multipart", "需使用 multipart/form-data 上报"))
			return
		}
		status := r.FormValue("status")
		stage := r.FormValue("stage")
		errMsg := r.FormValue("error")
		passAt1 := parseInt(r.FormValue("pass_at_1"))
		fixRounds := parseInt(r.FormValue("fix_rounds"))
		aiTokens := parseInt(r.FormValue("ai_tokens"))

		// 归属校验：只能上报自己认领的任务。
		var owner int64
		if err := d.DB.QueryRow(`SELECT worker_id FROM tasks WHERE id = ?`, id).Scan(&owner); err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
			return
		}
		if owner != wid {
			core.Fail(w, core.NewErr(http.StatusForbidden, "not_owner", "该任务不属于当前 worker"))
			return
		}

		if status != "failed" && status != "done" {
			core.Fail(w, core.Errf("bad_status", "status 仅支持 done / failed"))
			return
		}
		if status == "failed" {
			if stage == "" {
				stage = "build"
			}
			if errMsg == "" {
				errMsg = "构建失败"
			}
			if err := task.MarkFailed(r.Context(), d, id, wid, stage, errMsg); err != nil {
				core.Fail(w, err)
				return
			}
			core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "failed"})
			return
		}

		jarName, err := m.saveUpload(r, "jar", id, ".jar")
		if err != nil {
			core.Fail(w, err)
			return
		}
		logName, _ := m.saveUpload(r, "log", id, ".log")
		if jarName == "" {
			core.Fail(w, core.Errf("missing_jar", "未收到 jar 文件"))
			return
		}
		if err := task.MarkDone(r.Context(), d, id, wid, jarName, logName, passAt1, fixRounds, aiTokens); err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "done", "jar": jarName})
	}
}

// saveUpload 保存上传文件，返回落盘文件名（相对 DataDir）。
func (m *Module) saveUpload(r *http.Request, field string, taskID int64, ext string) (string, error) {
	f, _, err := r.FormFile(field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return "", nil
		}
		return "", core.Errf("bad_upload", "读取上传字段 %s 失败", field)
	}
	defer f.Close()
	if err := os.MkdirAll(m.DataDir, 0o755); err != nil {
		return "", err
	}
	name := "task-" + strconv.FormatInt(taskID, 10) + ext
	dst := filepath.Join(m.DataDir, name)
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(f, 64<<20)); err != nil {
		return "", err
	}
	return name, nil
}
