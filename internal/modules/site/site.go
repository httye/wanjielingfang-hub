// Package site 负责卫星站身份、配置下发与 AI 代理。
package site

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mcai/hub/internal/core"
)

const ddl = `
CREATE TABLE IF NOT EXISTS sites (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id         INTEGER NOT NULL,
	name            TEXT NOT NULL,
	site_key        TEXT NOT NULL UNIQUE,
	base_url        TEXT NOT NULL DEFAULT '',
	webhook_url     TEXT NOT NULL DEFAULT '',
	webhook_secret  TEXT NOT NULL DEFAULT '',
	api_version     INTEGER NOT NULL DEFAULT 1,
	site_version    TEXT NOT NULL DEFAULT '',
	config_override TEXT NOT NULL DEFAULT '',
	ai_tokens       INTEGER NOT NULL DEFAULT 0,
	ai_cost         INTEGER NOT NULL DEFAULT 0,
	revoked         INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL,
	rotated_at      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sites_user ON sites(user_id);
CREATE TABLE IF NOT EXISTS chat_sessions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    INTEGER NOT NULL,
	site_id    INTEGER NOT NULL DEFAULT 0,
	title      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_user ON chat_sessions(user_id, id DESC);
CREATE TABLE IF NOT EXISTS chat_messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id INTEGER NOT NULL,
	role       TEXT NOT NULL,
	content    TEXT NOT NULL,
	tokens     INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, id);
`

// Module 是卫星站模块。
type Module struct{}

// New 构造。
func New() *Module { return &Module{} }

// Name 模块名。
func (m *Module) Name() string { return "site" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 声明站点目录、界面、限额、功能开关。
func (m *Module) Defaults() map[string]map[string]any {
	return map[string]map[string]any{
		"catalog": {
			"platforms":   []any{"paper", "spigot", "bukkit", "fabric", "forge", "neoforge"},
			"mc_versions": []any{"1.8.8", "1.12.2", "1.16.5", "1.18.2", "1.20.4", "1.21.1"},
			"model_tiers": []any{"fast", "standard"},
		},
		"ui": {
			"modules":   []any{"generator", "market", "tasks"},
			"menus":     []any{"generate", "tasks", "market", "orders", "wallet", "sites", "settings"},
			"promotion": "",
			"i18n":      map[string]any{"zh": map[string]any{}, "en": map[string]any{}},
		},
		"limits": {
			"rate_per_min":     10,
			"max_active_tasks": 2,
			"build_timeout":    300,
			"artifact_days":    30,
			"max_src_bytes":    262144,
		},
		"features": {
			"free_generate": true,
			"shelf":         true,
			"market":        true,
			"subscription":  true,
			"invite":        true,
			"maintenance":   false,
		},
	}
}

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("POST /api/sites", d.Auth.RequireUser(m.create(d)))
	mux.HandleFunc("GET /api/sites", d.Auth.RequireUser(m.list(d)))
	mux.HandleFunc("POST /api/sites/{id}/revoke", d.Auth.RequireUser(m.revoke(d, true)))
	mux.HandleFunc("POST /api/sites/{id}/rotate", d.Auth.RequireUser(m.rotate(d)))
	mux.HandleFunc("GET /api/sites/{id}/usage", d.Auth.RequireUser(m.usage(d)))
	mux.HandleFunc("GET /api/sites/{id}/override", d.Auth.RequireUser(m.getOverride(d)))
	mux.HandleFunc("PUT /api/sites/{id}/override", d.Auth.RequireUser(m.putOverride(d)))

	// 卫星站专用（X-Site-Key）
	mux.HandleFunc("GET /api/v1/site/config", d.Auth.RequireSite(m.siteConfig(d)))
	mux.HandleFunc("POST /api/v1/site/ai/chat", d.Auth.RequireSite(m.aiChat(d)))
	mux.HandleFunc("POST /api/v1/site/ai/generate", d.Auth.RequireSite(m.aiGenerate(d)))
}

// ---------------------------------------------------------------------------
// 站点 CRUD
// ---------------------------------------------------------------------------

func (m *Module) create(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string `json:"name"`
			BaseURL    string `json:"base_url"`
			WebhookURL string `json:"webhook_url"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			req.Name = "新站点"
		}
		// base_url 是 SSO 回跳白名单的来源，必须是纯 origin；带路径或查询串等于开了个任意跳转口子。
		if req.BaseURL != "" {
			pu, perr := url.Parse(req.BaseURL)
			if perr != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" ||
				(pu.Path != "" && pu.Path != "/") || pu.RawQuery != "" || pu.Fragment != "" {
				core.Fail(w, core.Errf("bad_base_url", "base_url 必须是纯 origin，例如 https://mc.example.com"))
				return
			}
			req.BaseURL = pu.Scheme + "://" + pu.Host
		}
		if req.WebhookURL != "" {
			pu, perr := url.Parse(req.WebhookURL)
			if perr != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
				core.Fail(w, core.Errf("bad_webhook", "webhook_url 必须是 http(s) 地址"))
				return
			}
		}
		u := core.UserFrom(r)
		now := d.Unix()
		var n int64
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM sites WHERE user_id = ? AND revoked = 0`, u.ID).Scan(&n)
		if n >= 10 {
			core.Fail(w, core.NewErr(http.StatusConflict, "too_many_sites", "单账号最多 10 个站点"))
			return
		}
		key := core.SlugKey()
		secret := core.RandomToken(24)
		res, err := d.DB.Exec(`INSERT INTO sites(user_id, name, site_key, base_url, webhook_url, webhook_secret,
			api_version, site_version, config_override, revoked, created_at)
			VALUES(?,?,?,?,?,?,1,'','',0,?)`,
			u.ID, req.Name, key, strings.TrimRight(req.BaseURL, "/"), req.WebhookURL, secret, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, u.ID, "site_create", req.Name, "", core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{
			"id": id, "name": req.Name, "site_key": key, "webhook_secret": secret,
			"base_url": req.BaseURL, "api_version": 1,
		})
	}
}

func (m *Module) list(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		rows, err := d.DB.Query(`SELECT id, name, site_key, base_url, webhook_url, api_version, site_version,
			ai_tokens, ai_cost, revoked, created_at FROM sites WHERE user_id = ? ORDER BY id DESC`, u.ID)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, apiVersion, tokens, cost, revoked, created int64
				name, key, base, hook, siteVer                 string
			)
			if rows.Scan(&id, &name, &key, &base, &hook, &apiVersion, &siteVer, &tokens, &cost, &revoked, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "name": name, "site_key": key, "base_url": base, "webhook_url": hook,
				"api_version": apiVersion, "site_version": siteVer,
				"ai_tokens": tokens, "ai_cost": cost, "revoked": revoked == 1, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) ownSite(d *core.Deps, r *http.Request) (int64, error) {
	u := core.UserFrom(r)
	id, err := core.PathInt(r, "id")
	if err != nil {
		return 0, err
	}
	var owner int64
	if err := d.DB.QueryRow(`SELECT user_id FROM sites WHERE id = ?`, id).Scan(&owner); err != nil {
		return 0, core.NewErr(http.StatusNotFound, "not_found", "站点不存在")
	}
	if owner != u.ID && !core.IsAdmin(u) {
		return 0, core.NewErr(http.StatusForbidden, "forbidden", "无权操作该站点")
	}
	return id, nil
}

func (m *Module) revoke(d *core.Deps, on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.ownSite(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		flag := 0
		if on {
			flag = 1
		}
		if _, err := d.DB.Exec(`UPDATE sites SET revoked = ? WHERE id = ?`, flag, id); err != nil {
			core.Fail(w, err)
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "site_revoke", fmt.Sprint(id), fmt.Sprint(on), core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": on})
	}
}

func (m *Module) rotate(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.ownSite(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		key := core.SlugKey()
		secret := core.RandomToken(24)
		now := d.Unix()
		if _, err := d.DB.Exec(`UPDATE sites SET site_key = ?, webhook_secret = ?, rotated_at = ? WHERE id = ?`,
			key, secret, now, id); err != nil {
			core.Fail(w, err)
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "site_rotate", fmt.Sprint(id), "", core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "site_key": key, "webhook_secret": secret})
	}
}

func (m *Module) usage(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.ownSite(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		var (
			tasks, done, failed, tokens, cost int64
		)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE site_id = ?`, id).Scan(&tasks)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE site_id = ? AND status = 'done'`, id).Scan(&done)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE site_id = ? AND status = 'failed'`, id).Scan(&failed)
		_ = d.DB.QueryRow(`SELECT ai_tokens, ai_cost FROM sites WHERE id = ?`, id).Scan(&tokens, &cost)
		core.WriteJSON(w, http.StatusOK, map[string]any{
			"site_id": id, "tasks": tasks, "done": done, "failed": failed,
			"ai_tokens": tokens, "ai_cost": cost,
		})
	}
}

func (m *Module) getOverride(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.ownSite(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		var ov string
		_ = d.DB.QueryRow(`SELECT config_override FROM sites WHERE id = ?`, id).Scan(&ov)
		core.WriteJSON(w, http.StatusOK, map[string]any{"site_id": id, "override": ov})
	}
}

func (m *Module) putOverride(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.ownSite(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		var body struct {
			Override string `json:"override"`
		}
		if err := core.ReadJSON(r, &body); err != nil {
			core.Fail(w, err)
			return
		}
		// 空字符串表示清空覆盖，恢复纯平台默认。
		if strings.TrimSpace(body.Override) == "" {
			if _, err := d.DB.Exec(`UPDATE sites SET config_override = '' WHERE id = ?`, id); err != nil {
				core.Fail(w, err)
				return
			}
			core.Audit(d.DB, core.UserFrom(r).ID, "site_override_clear", fmt.Sprint(id), "", core.ClientIP(r), d.Unix())
			core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": true})
			return
		}

		// 展示类分区 + 计费：站长可以给自己的站点定价，但受中心站下限约束。
		allowed := map[string]bool{"ui": true, "brand": true, "catalog": true, "billing": true}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(body.Override), &parsed); err != nil {
			core.Fail(w, core.Errf("bad_override", "override 必须是 JSON 对象"))
			return
		}
		for k := range parsed {
			if !allowed[k] {
				core.Fail(w, core.Errf("bad_section", "不允许覆盖分区: %s", k))
				return
			}
		}
		if raw, ok := parsed["billing"]; ok {
			billing, ok := raw.(map[string]any)
			if !ok {
				core.Fail(w, core.Errf("bad_billing", "billing 必须是对象"))
				return
			}
			minPrice := d.Cfg.Int("billing", "min_task_cost", 1)
			allowedKeys := map[string]bool{
				"task_cost": true, "iterate_cost": true, "credit_cost_fail": true,
			}
			for k, v := range billing {
				if !allowedKeys[k] {
					core.Fail(w, core.Errf("bad_billing_key",
						"billing 只允许改 task_cost / iterate_cost / credit_cost_fail，不支持 %s", k))
					return
				}
				n, ok := toInt64(v)
				if !ok || n < 0 {
					core.Fail(w, core.Errf("bad_billing_value", "%s 必须是非负整数", k))
					return
				}
				if k != "credit_cost_fail" && n < minPrice {
					core.Fail(w, core.Errf("price_too_low",
						"%s 不能低于平台下限 %d 积分", k, minPrice))
					return
				}
			}
		}
		if _, err := d.DB.Exec(`UPDATE sites SET config_override = ? WHERE id = ?`, body.Override, id); err != nil {
			core.Fail(w, err)
			return
		}
		core.Audit(d.DB, core.UserFrom(r).ID, "site_override", fmt.Sprint(id), body.Override, core.ClientIP(r), d.Unix())
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// toInt64 把 JSON 数字转换成 int64。
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), n == float64(int64(n))
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 配置下发
// ---------------------------------------------------------------------------

func (m *Module) siteConfig(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := core.SiteFrom(r)
		ver, all, err := d.Cfg.Snapshot()
		if err != nil {
			core.Fail(w, err)
			return
		}
		var (
			override, baseURL string
			apiVersion        int64
		)
		if err := d.DB.QueryRow(`SELECT config_override, base_url, api_version FROM sites WHERE id = ?`, s.ID).
			Scan(&override, &baseURL, &apiVersion); err != nil {
			core.Fail(w, err)
			return
		}

		etag := fmt.Sprintf(`"v%d.s%d"`, ver, s.ID)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		since := core.QueryInt(r, "since", 0)
		full := r.URL.Query().Get("full") == "1"
		if !full && since > 0 && since == ver {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		merged := core.MergeSiteConfig(all, override)
		out := map[string]any{
			"config_version": ver,
			"site": map[string]any{
				"id":          s.ID,
				"name":        s.Name,
				"base_url":    baseURL,
				"api_version": apiVersion,
			},
			"server_time": d.Unix(),
		}
		for section, kv := range merged {
			out[section] = kv
		}
		core.WriteJSON(w, http.StatusOK, out)
	}
}
