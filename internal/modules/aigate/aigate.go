// Package aigate 把 hub 变成一个 OpenAI 兼容的 AI 中转网关：
// 外部工具只配 base_url + hub 签发的 key，密钥与渠道全在中心站，按渠道做故障转移与用量统计。
package aigate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mcai/hub/internal/core"
)

const ddl = `
CREATE TABLE IF NOT EXISTS ai_channels (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	base_url   TEXT NOT NULL,
	api_key    TEXT NOT NULL DEFAULT '',
	model      TEXT NOT NULL DEFAULT '',
	priority   INTEGER NOT NULL DEFAULT 0,
	enabled    INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS ai_usage (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id           INTEGER NOT NULL DEFAULT 0,
	site_id           INTEGER NOT NULL DEFAULT 0,
	channel_id        INTEGER NOT NULL DEFAULT 0,
	model             TEXT NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens      INTEGER NOT NULL DEFAULT 0,
	status            INTEGER NOT NULL DEFAULT 200,
	created_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ai_usage_user ON ai_usage(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_ai_usage_created ON ai_usage(created_at DESC);
CREATE TABLE IF NOT EXISTS ai_keys (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id      INTEGER NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	api_key      TEXT NOT NULL UNIQUE,
	enabled      INTEGER NOT NULL DEFAULT 1,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ai_keys_user ON ai_keys(user_id, id DESC);
`

// Module 是 AI 中转网关。
type Module struct{}

// New 构造。
func New() *Module { return &Module{} }

// Name 模块名。
func (m *Module) Name() string { return "aigate" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 声明中转相关配置。
func (m *Module) Defaults() map[string]map[string]any {
	return map[string]map[string]any{
		"ai": {
			"enabled":           true,
			"daily_token_limit": 0, // 0 = 不限
			"default_model":     "",
		},
	}
}

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	// OpenAI 兼容入口
	mux.HandleFunc("GET /v1/models", m.listModels(d))
	mux.HandleFunc("POST /v1/chat/completions", m.chat(d))
	mux.HandleFunc("GET /v1/usage", m.myUsage(d))

	// 用户自助管理 API key
	mux.HandleFunc("GET /api/v1/ai/keys", d.Auth.RequireUser(m.listKeys(d)))
	mux.HandleFunc("POST /api/v1/ai/keys", d.Auth.RequireUser(m.createKey(d)))
	mux.HandleFunc("POST /api/v1/ai/keys/{id}/revoke", d.Auth.RequireUser(m.revokeKey(d)))

	// 后台渠道管理
	mux.HandleFunc("GET /api/admin/ai/channels", d.Auth.RequireAdmin(m.listChannels(d)))
	mux.HandleFunc("POST /api/admin/ai/channels", d.Auth.RequireAdmin(m.createChannel(d)))
	mux.HandleFunc("POST /api/admin/ai/channels/{id}", d.Auth.RequireAdmin(m.updateChannel(d)))
	mux.HandleFunc("POST /api/admin/ai/channels/{id}/delete", d.Auth.RequireAdmin(m.deleteChannel(d)))
	mux.HandleFunc("GET /api/admin/ai/usage", d.Auth.RequireAdmin(m.adminUsage(d)))
}

// ---------------------------------------------------------------------------
// 身份
// ---------------------------------------------------------------------------

type identity struct {
	UserID int64
	SiteID int64
	KeyID  int64
	Via    string
}

// identify 支持三种凭据：hub 签发的 AI key、用户 JWT、站点 key。
func (m *Module) identify(d *core.Deps, r *http.Request) (*identity, error) {
	if k := r.Header.Get("X-API-Key"); k != "" {
		var id, uid int64
		var enabled int
		err := d.DB.QueryRow(`SELECT id, user_id, enabled FROM ai_keys WHERE api_key = ?`, k).Scan(&id, &uid, &enabled)
		if err != nil || enabled != 1 {
			return nil, core.NewErr(http.StatusUnauthorized, "bad_api_key", "API key 无效或已停用")
		}
		_, _ = d.DB.Exec(`UPDATE ai_keys SET last_used_at = ? WHERE id = ?`, d.Unix(), id)
		return &identity{UserID: uid, KeyID: id, Via: "api_key"}, nil
	}
	if k := r.Header.Get("X-Site-Key"); k != "" {
		var sid, uid int64
		var revoked int
		err := d.DB.QueryRow(`SELECT id, user_id, revoked FROM sites WHERE site_key = ?`, k).Scan(&sid, &uid, &revoked)
		if err != nil || revoked == 1 {
			return nil, core.NewErr(http.StatusUnauthorized, "bad_site_key", "站点 key 无效")
		}
		return &identity{UserID: uid, SiteID: sid, Via: "site_key"}, nil
	}
	if tok := d.Auth.TokenFrom(r); tok != "" {
		u, err := d.Auth.MustResolve(r)
		if err != nil {
			return nil, err
		}
		return &identity{UserID: u.ID, Via: "jwt"}, nil
	}
	return nil, core.NewErr(http.StatusUnauthorized, "unauthorized", "需要 X-API-Key / X-Site-Key 或登录态")
}

// ---------------------------------------------------------------------------
// 渠道
// ---------------------------------------------------------------------------

type channel struct {
	ID       int64
	Name     string
	BaseURL  string
	APIKey   string
	Model    string
	Priority int64
}

// channels 返回启用的渠道；没配任何渠道时回落到环境变量里的默认上游。
func (m *Module) channels(d *core.Deps) []channel {
	rows, err := d.DB.Query(`SELECT id, name, base_url, api_key, model, priority
		FROM ai_channels WHERE enabled = 1 ORDER BY priority DESC, id ASC`)
	if err == nil {
		defer rows.Close()
		var out []channel
		for rows.Next() {
			var c channel
			if rows.Scan(&c.ID, &c.Name, &c.BaseURL, &c.APIKey, &c.Model, &c.Priority) == nil {
				out = append(out, c)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if d.AI.BaseURL == "" {
		return nil
	}
	return []channel{{
		ID: 0, Name: "env", BaseURL: d.AI.BaseURL, APIKey: d.AI.APIKey, Model: d.AI.Model, Priority: 0,
	}}
}

func (m *Module) listModels(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := m.identify(d, r); err != nil {
			core.Fail(w, err)
			return
		}
		seen := map[string]bool{}
		data := []map[string]any{}
		add := func(name string) {
			if name == "" || seen[name] {
				return
			}
			seen[name] = true
			data = append(data, map[string]any{
				"id": name, "object": "model", "created": 0, "owned_by": "mcai-hub",
			})
		}
		for _, c := range m.channels(d) {
			add(c.Model)
		}
		for _, t := range toStrings(d.Cfg.Section("catalog")["model_tiers"]) {
			add(t)
		}
		add(d.Cfg.Str("ai", "default_model", ""))
		core.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	}
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
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

// ---------------------------------------------------------------------------
// 对话转发
// ---------------------------------------------------------------------------

func (m *Module) chat(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.Cfg.Bool("ai", "enabled", true) {
			core.Fail(w, core.NewErr(http.StatusForbidden, "ai_disabled", "AI 中转已关闭"))
			return
		}
		id, err := m.identify(d, r)
		if err != nil {
			// 未读完请求体就返回会触发 TCP RST，客户端拿不到 401 文本。
			drainBody(r)
			core.Fail(w, err)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusBadRequest, "bad_body", "读取请求体失败"))
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			core.Fail(w, core.NewErr(http.StatusBadRequest, "bad_json", "请求体不是合法 JSON"))
			return
		}
		stream, _ := payload["stream"].(bool)
		reqModel, _ := payload["model"].(string)

		// 每日 token 限额
		if limit := d.Cfg.Int("ai", "daily_token_limit", 0); limit > 0 {
			var used int64
			_ = d.DB.QueryRow(`SELECT COALESCE(SUM(total_tokens),0) FROM ai_usage
				WHERE user_id = ? AND created_at >= ?`, id.UserID, startOfDay(d.Unix())).Scan(&used)
			if used >= limit {
				core.Fail(w, core.NewErr(http.StatusTooManyRequests, "quota_exceeded",
					fmt.Sprintf("今日 token 已用 %d / %d", used, limit)))
				return
			}
		}

		chs := m.channels(d)
		if len(chs) == 0 && !d.AI.Mock {
			core.Fail(w, core.NewErr(http.StatusServiceUnavailable, "no_channel", "还没有可用的 AI 渠道"))
			return
		} else if d.AI.Mock {
			// AI_MOCK=1：不真的打上游，返回内置响应，方便本地把链路跑通。
			m.mockResponse(w, d, id, modelOr(reqModel, d), stream)
			return
		}

		var lastErr string
		for _, ch := range chs {
			upBody := body
			model := reqModel
			if ch.Model != "" {
				model = ch.Model
				payload["model"] = model
				if b, err := json.Marshal(payload); err == nil {
					upBody = b
				}
			}
			if stream {
				// 让上游一定回 usage，否则中转侧统计不到
				var m2 map[string]any
				if json.Unmarshal(upBody, &m2) == nil {
					m2["stream_options"] = map[string]any{"include_usage": true}
					if b, err := json.Marshal(m2); err == nil {
						upBody = b
					}
				}
			}

			resp, err := m.doUpstream(r.Context(), ch, upBody, d)
			if err != nil {
				lastErr = err.Error()
				continue
			}
			if resp.StatusCode >= 400 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				resp.Body.Close()
				lastErr = fmt.Sprintf("%s: %d %s", ch.Name, resp.StatusCode, strings.TrimSpace(string(raw)))
				continue
			}

			if stream {
				m.pipeStream(w, r, resp, d, id, ch, model)
			} else {
				m.pipeOnce(w, resp, d, id, ch, model)
			}
			return
		}

		core.Fail(w, core.NewErr(http.StatusBadGateway, "upstream_failed", "上游全部失败: "+lastErr))
	}
}

func (m *Module) doUpstream(ctx context.Context, ch channel, body []byte, d *core.Deps) (*http.Response, error) {
	url := strings.TrimRight(ch.BaseURL, "/")
	if !strings.HasSuffix(url, "/chat/completions") {
		url += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if ch.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ch.APIKey)
	}
	return d.HTTP.Do(req)
}

func (m *Module) pipeOnce(w http.ResponseWriter, resp *http.Response, d *core.Deps, id *identity, ch channel, model string) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		core.Fail(w, core.NewErr(http.StatusBadGateway, "upstream_read", "读取上游响应失败"))
		return
	}
	var parsed struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	var pt, ct, tt int64
	if json.Unmarshal(raw, &parsed) == nil && parsed.Usage != nil {
		pt, ct, tt = parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, parsed.Usage.TotalTokens
		if tt == 0 {
			tt = pt + ct
		}
	}
	m.record(d, id, ch, model, pt, ct, tt, resp.StatusCode)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (m *Module) pipeStream(w http.ResponseWriter, r *http.Request, resp *http.Response, d *core.Deps, id *identity, ch channel, model string) {
	defer resp.Body.Close()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var pt, ct, tt int64
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if _, err := io.WriteString(w, line+"\n\n"); err != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			pt, ct, tt = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens
		}
	}
	m.record(d, id, ch, model, pt, ct, tt, 200)
}

func (m *Module) record(d *core.Deps, id *identity, ch channel, model string, pt, ct, tt int64, status int) {
	if tt == 0 && pt == 0 && ct == 0 {
		return
	}
	_, _ = d.DB.Exec(`INSERT INTO ai_usage(user_id, site_id, channel_id, model, prompt_tokens, completion_tokens,
		total_tokens, status, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id.UserID, id.SiteID, ch.ID, model, pt, ct, tt, status, d.Unix())
	if id.SiteID > 0 {
		_, _ = d.DB.Exec(`UPDATE sites SET ai_tokens = ai_tokens + ? WHERE id = ?`, tt, id.SiteID)
	}
}

// mockResponse 在 AI_MOCK=1 时返回内置响应，让本地无需真实上游也能验证链路。
func (m *Module) mockResponse(w http.ResponseWriter, d *core.Deps, id *identity, model string, stream bool) {
	const pt, ct = int64(12), int64(8)
	ch := channel{ID: 0, Name: "mock"}

	if stream {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache, no-transform")
		h.Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)
		for _, part := range []string{"你好", "，我是 ", "hub 内置 mock 模型", "。"} {
			chunk := map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": part}}},
			}
			b, _ := json.Marshal(chunk)
			_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		ub, _ := json.Marshal(map[string]any{"usage": map[string]any{
			"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct,
		}})
		_, _ = io.WriteString(w, "data: "+string(ub)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		m.record(d, id, ch, model, pt, ct, pt+ct, 200)
		return
	}

	m.record(d, id, ch, model, pt, ct, pt+ct, 200)
	core.WriteJSON(w, http.StatusOK, map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "你好，我是 hub 内置的 mock 模型（AI_MOCK=1）。"},
		}},
		"usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct},
	})
}

func modelOr(model string, d *core.Deps) string {
	if model != "" {
		return model
	}
	if v := d.Cfg.Str("ai", "default_model", ""); v != "" {
		return v
	}
	if d.AI.Model != "" {
		return d.AI.Model
	}
	return "mock-model"
}

func startOfDay(unix int64) int64 {
	t := time.Unix(unix, 0).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

// drainBody 丢弃剩余请求体，避免服务端提前关连接时触发 RST。
func drainBody(r *http.Request) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	}
}

// ---------------------------------------------------------------------------
// 用量查询
// ---------------------------------------------------------------------------

func (m *Module) myUsage(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := m.identify(d, r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		since := d.Unix() - 7*86400
		rows, err := d.DB.Query(`SELECT date(created_at,'unixepoch') AS day, COUNT(1),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0)
			FROM ai_usage WHERE user_id = ? AND created_at >= ? GROUP BY day ORDER BY day DESC`, id.UserID, since)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var day string
			var calls, pt, ct, tt int64
			if rows.Scan(&day, &calls, &pt, &ct, &tt) == nil {
				items = append(items, map[string]any{
					"day": day, "calls": calls, "prompt_tokens": pt,
					"completion_tokens": ct, "total_tokens": tt,
				})
			}
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "days": 7})
	}
}

func (m *Module) adminUsage(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		since := d.Unix() - 7*86400
		rows, err := d.DB.Query(`SELECT c.name, COUNT(1), COALESCE(SUM(u.total_tokens),0)
			FROM ai_usage u LEFT JOIN ai_channels c ON c.id = u.channel_id
			WHERE u.created_at >= ? GROUP BY u.channel_id ORDER BY 3 DESC`, since)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		byChannel := []map[string]any{}
		for rows.Next() {
			var name sql.NullString
			var calls, tokens int64
			if rows.Scan(&name, &calls, &tokens) == nil {
				n := name.String
				if n == "" {
					n = "默认渠道"
				}
				byChannel = append(byChannel, map[string]any{
					"channel": n, "calls": calls, "total_tokens": tokens,
				})
			}
		}

		var totalCalls, totalTokens, users int64
		_ = d.DB.QueryRow(`SELECT COUNT(1), COALESCE(SUM(total_tokens),0), COUNT(DISTINCT user_id)
			FROM ai_usage WHERE created_at >= ?`, since).Scan(&totalCalls, &totalTokens, &users)

		core.WriteJSON(w, http.StatusOK, map[string]any{
			"window_days": 7, "calls": totalCalls, "total_tokens": totalTokens,
			"active_users": users, "by_channel": byChannel,
		})
	}
}

// ---------------------------------------------------------------------------
// API Key
// ---------------------------------------------------------------------------

func (m *Module) listKeys(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		rows, err := d.DB.Query(`SELECT id, name, api_key, enabled, created_at, last_used_at
			FROM ai_keys WHERE user_id = ? ORDER BY id DESC`, u.ID)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, enabled, created, last int64
				name, key                  string
			)
			if rows.Scan(&id, &name, &key, &enabled, &created, &last) == nil {
				masked := key
				if len(key) > 10 {
					masked = key[:8] + "..." + key[len(key)-4:]
				}
				items = append(items, map[string]any{
					"id": id, "name": name, "api_key": masked, "enabled": enabled == 1,
					"created_at": created, "last_used_at": last,
				})
			}
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) createKey(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		_ = core.ReadJSON(r, &req)
		u := core.UserFrom(r)
		now := d.Unix()
		var n int64
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM ai_keys WHERE user_id = ? AND enabled = 1`, u.ID).Scan(&n)
		if n >= 5 {
			core.Fail(w, core.NewErr(http.StatusConflict, "too_many_keys", "最多同时保留 5 个 key"))
			return
		}
		key := "ak_" + core.RandomToken(24)
		if req.Name == "" {
			req.Name = "默认 key"
		}
		res, err := d.DB.Exec(`INSERT INTO ai_keys(user_id, name, api_key, enabled, created_at) VALUES(?,?,?,1,?)`,
			u.ID, req.Name, key, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, u.ID, "ai_key_create", req.Name, "", core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name, "api_key": key})
	}
}

func (m *Module) revokeKey(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		res, err := d.DB.Exec(`UPDATE ai_keys SET enabled = 0 WHERE id = ? AND user_id = ?`, id, u.ID)
		if err != nil {
			core.Fail(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "key 不存在"))
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ---------------------------------------------------------------------------
// 后台渠道管理
// ---------------------------------------------------------------------------

func (m *Module) listChannels(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(`SELECT id, name, base_url, api_key, model, priority, enabled, created_at
			FROM ai_channels ORDER BY priority DESC, id ASC`)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, priority, enabled, created int64
				name, base, key, model         string
			)
			if rows.Scan(&id, &name, &base, &key, &model, &priority, &enabled, &created) == nil {
				masked := ""
				if len(key) >= 4 {
					masked = "••••" + key[len(key)-4:]
				} else if key != "" {
					masked = "••••"
				}
				items = append(items, map[string]any{
					"id": id, "name": name, "base_url": base, "api_key": masked, "model": model,
					"priority": priority, "enabled": enabled == 1, "created_at": created,
				})
			}
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "has_env": d.AI.BaseURL != ""})
	}
}

func (m *Module) createChannel(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name     string `json:"name"`
			BaseURL  string `json:"base_url"`
			APIKey   string `json:"api_key"`
			Model    string `json:"model"`
			Priority int64  `json:"priority"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.Name == "" || req.BaseURL == "" {
			core.Fail(w, core.Errf("bad_channel", "名称与 Base URL 必填"))
			return
		}
		if !strings.HasPrefix(req.BaseURL, "http://") && !strings.HasPrefix(req.BaseURL, "https://") {
			core.Fail(w, core.Errf("bad_url", "Base URL 必须是 http(s) 地址"))
			return
		}
		now := d.Unix()
		res, err := d.DB.Exec(`INSERT INTO ai_channels(name, base_url, api_key, model, priority, enabled, created_at, updated_at)
			VALUES(?,?,?,?,?,1,?,?)`, req.Name, req.BaseURL, req.APIKey, req.Model, req.Priority, now, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.Audit(d.DB, core.UserFrom(r).ID, "ai_channel_create", req.Name, req.BaseURL, core.ClientIP(r), now)
		core.WriteJSON(w, http.StatusCreated, map[string]any{"id": id})
	}
}

func (m *Module) updateChannel(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		var req struct {
			Name     *string `json:"name"`
			BaseURL  *string `json:"base_url"`
			APIKey   *string `json:"api_key"`
			Model    *string `json:"model"`
			Priority *int64  `json:"priority"`
			Enabled  *bool   `json:"enabled"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		set := []string{}
		args := []any{}
		if req.Name != nil {
			set = append(set, "name = ?")
			args = append(args, *req.Name)
		}
		if req.BaseURL != nil {
			set = append(set, "base_url = ?")
			args = append(args, *req.BaseURL)
		}
		if req.APIKey != nil && *req.APIKey != "" {
			set = append(set, "api_key = ?")
			args = append(args, *req.APIKey)
		}
		if req.Model != nil {
			set = append(set, "model = ?")
			args = append(args, *req.Model)
		}
		if req.Priority != nil {
			set = append(set, "priority = ?")
			args = append(args, *req.Priority)
		}
		if req.Enabled != nil {
			v := 0
			if *req.Enabled {
				v = 1
			}
			set = append(set, "enabled = ?")
			args = append(args, v)
		}
		if len(set) == 0 {
			core.Fail(w, core.Errf("no_change", "没有要更新的字段"))
			return
		}
		set = append(set, "updated_at = ?")
		args = append(args, d.Unix(), id)
		if _, err := d.DB.Exec(`UPDATE ai_channels SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...); err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (m *Module) deleteChannel(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		if _, err := d.DB.Exec(`DELETE FROM ai_channels WHERE id = ?`, id); err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
