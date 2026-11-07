// Package account 负责用户、登录态、SSO 与托管认证页。
package account

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/mcai/hub/internal/core"
)

const ddl = `
CREATE TABLE IF NOT EXISTS users (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	email          TEXT NOT NULL UNIQUE,
	username       TEXT NOT NULL UNIQUE,
	pass_hash      TEXT NOT NULL,
	role           TEXT NOT NULL DEFAULT 'user',
	balance        INTEGER NOT NULL DEFAULT 0,
	signup_granted INTEGER NOT NULL DEFAULT 0,
	invite_code    TEXT NOT NULL UNIQUE,
	invited_by     INTEGER NOT NULL DEFAULT 0,
	vip_until      INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sso_codes (
	code       TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL,
	redirect   TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	used       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sso_expires ON sso_codes(expires_at)
`

// Module 是用户与认证模块。
type Module struct{}

// New 构造模块。
func New() *Module { return &Module{} }

// Name 模块名。
func (m *Module) Name() string { return "account" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 声明本模块负责的配置分区。
func (m *Module) Defaults() map[string]map[string]any {
	return map[string]map[string]any{
		"brand": {
			"name":    "万界零方-中心hub",
			"logo":    "",
			"primary": "#3b82f6",
			"support": "",
			"icp":     "",
		},
		"auth": {
			"register_open":   true,
			"email_verify":    false,
			"invite_required": false,
			"password_min":    8,
			"hosted_login":    "/auth/login",
			"hosted_register": "/auth/register",
			"redirect_allow":  []any{},
		},
	}
}

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("POST /api/register", m.register(d))
	mux.HandleFunc("POST /api/login", m.login(d))
	mux.HandleFunc("POST /api/logout", m.logout(d))
	mux.HandleFunc("GET /api/me", d.Auth.RequireUser(m.me(d)))
	mux.HandleFunc("GET /api/v1/me/overview", d.Auth.RequireUser(m.overview(d)))
	mux.HandleFunc("GET /sso", m.sso(d))
	mux.HandleFunc("POST /api/sso/exchange", m.ssoExchange(d))
	mux.HandleFunc("GET /auth/login", m.page(d, "login"))
	mux.HandleFunc("GET /auth/register", m.page(d, "register"))
	mux.HandleFunc("POST /auth/login", m.formLogin(d))
	mux.HandleFunc("POST /auth/register", m.formRegister(d))
}

// ---------------------------------------------------------------------------
// 请求体
// ---------------------------------------------------------------------------

type registerReq struct {
	Email      string `json:"email"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	InviteCode string `json:"invite_code"`
}

type loginReq struct {
	Identifier string `json:"identifier"`
	Email      string `json:"email"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

var (
	reEmail = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
	reUser  = regexp.MustCompile(`^[A-Za-z0-9_]{3,24}$`)
)

// ---------------------------------------------------------------------------
// 注册 / 登录
// ---------------------------------------------------------------------------

func (m *Module) register(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerReq
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		u, token, err := m.createUser(d, r.Context(), req, core.ClientIP(r))
		if err != nil {
			core.Fail(w, err)
			return
		}
		d.Auth.SetCookie(w, token)
		core.WriteJSON(w, http.StatusCreated, map[string]any{"token": token, "user": m.publicUser(d, u.ID)})
	}
}

func (m *Module) formRegister(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/auth/register?err=bad+form", http.StatusFound)
			return
		}
		redirect := safeRedirect(d, m, r.PostFormValue("redirect"))
		req := registerReq{
			Email:      strings.TrimSpace(r.PostFormValue("email")),
			Username:   strings.TrimSpace(r.PostFormValue("username")),
			Password:   r.PostFormValue("password"),
			InviteCode: strings.TrimSpace(r.PostFormValue("invite_code")),
		}
		_, token, err := m.createUser(d, r.Context(), req, core.ClientIP(r))
		if err != nil {
			msg := "注册失败"
			var ae *core.APIError
			if errors.As(err, &ae) {
				msg = ae.Msg
			}
			http.Redirect(w, r, "/auth/register?err="+url.QueryEscape(msg), http.StatusFound)
			return
		}
		d.Auth.SetCookie(w, token)
		http.Redirect(w, r, redirect, http.StatusFound)
	}
}

func (m *Module) createUser(d *core.Deps, ctx context.Context, req registerReq, ip string) (*core.User, string, error) {
	if !d.Cfg.Bool("auth", "register_open", true) {
		return nil, "", core.NewErr(http.StatusForbidden, "register_closed", "当前未开放注册")
	}
	if ok, _, _ := core.Allow(d.DB, "reg:ip:"+ip, 10, 3600, d.Unix()); !ok {
		return nil, "", core.NewErr(http.StatusTooManyRequests, "rate_limited", "注册过于频繁，请稍后再试")
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Username = strings.TrimSpace(req.Username)
	if !reEmail.MatchString(req.Email) {
		return nil, "", core.Errf("bad_email", "邮箱格式不正确")
	}
	if !reUser.MatchString(req.Username) {
		return nil, "", core.Errf("bad_username", "用户名需 3-24 位字母、数字或下划线")
	}
	minLen := int(d.Cfg.Int("auth", "password_min", 8))
	if len([]rune(req.Password)) < minLen {
		return nil, "", core.Errf("weak_password", "密码至少 %d 位", minLen)
	}
	if d.Cfg.Bool("auth", "invite_required", false) && req.InviteCode == "" {
		return nil, "", core.Errf("invite_required", "需要邀请码")
	}

	hash, err := d.Auth.HashPassword(req.Password)
	if err != nil {
		return nil, "", err
	}

	now := d.Unix()
	var userID int64
	err = core.Tx(ctx, d.DB, func(tx *sql.Tx) error {
		invitedBy := int64(0)
		if req.InviteCode != "" {
			_ = tx.QueryRow(`SELECT id FROM users WHERE invite_code = ?`, strings.ToUpper(req.InviteCode)).Scan(&invitedBy)
		}
		code, cerr := uniqueInviteCode(tx)
		if cerr != nil {
			return cerr
		}
		res, ierr := tx.Exec(`INSERT INTO users(email, username, pass_hash, role, balance, signup_granted,
			invite_code, invited_by, vip_until, created_at, updated_at)
			VALUES(?,?,?,'user',0,0,?,?,0,?,?)`, req.Email, req.Username, hash, code, invitedBy, now, now)
		if ierr != nil {
			if strings.Contains(ierr.Error(), "UNIQUE") {
				return core.NewErr(http.StatusConflict, "duplicate", "邮箱或用户名已被占用")
			}
			return ierr
		}
		userID, _ = res.LastInsertId()

		if invitedBy > 0 {
			bonus := d.Cfg.Int("billing", "referral_bonus", 10)
			if bonus > 0 {
				if _, berr := core.AddBalance(tx, invitedBy, bonus, "referral", "user", userID, 0, now); berr != nil {
					return berr
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}

	u := &core.User{ID: userID, Email: req.Email, Role: "user", Balance: 0}
	token, err := d.Auth.Issue(u)
	if err != nil {
		return nil, "", err
	}
	core.Audit(d.DB, userID, "register", req.Email, "invited_by="+itoa(invitedIDOf(d, userID)), ip, now)
	return u, token, nil
}

// invitedIDOf 只用于审计文案，取不到就返回 0。
func invitedIDOf(d *core.Deps, userID int64) int64 {
	var v int64
	_ = d.DB.QueryRow(`SELECT invited_by FROM users WHERE id = ?`, userID).Scan(&v)
	return v
}

func uniqueInviteCode(tx *sql.Tx) (string, error) {
	for i := 0; i < 6; i++ {
		code := core.RandomCode(3)
		var n int
		if err := tx.QueryRow(`SELECT COUNT(1) FROM users WHERE invite_code = ?`, code).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return code, nil
		}
	}
	return "", errors.New("invite code collision")
}

func (m *Module) login(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginReq
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.Identifier == "" {
			req.Identifier = req.Email
		}
		if req.Identifier == "" {
			req.Identifier = req.Username
		}
		u, token, err := m.authenticate(d, req.Identifier, req.Password, core.ClientIP(r))
		if err != nil {
			core.Fail(w, err)
			return
		}
		d.Auth.SetCookie(w, token)
		core.WriteJSON(w, http.StatusOK, map[string]any{"token": token, "user": m.publicUser(d, u.ID)})
	}
}

func (m *Module) formLogin(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/auth/login?err=bad+form", http.StatusFound)
			return
		}
		redirect := safeRedirect(d, m, r.PostFormValue("redirect"))
		_, token, err := m.authenticate(d, r.PostFormValue("identifier"), r.PostFormValue("password"), core.ClientIP(r))
		if err != nil {
			msg := "登录失败"
			var ae *core.APIError
			if errors.As(err, &ae) {
				msg = ae.Msg
			}
			http.Redirect(w, r, "/auth/login?err="+url.QueryEscape(msg)+"&redirect="+url.QueryEscape(redirect), http.StatusFound)
			return
		}
		d.Auth.SetCookie(w, token)
		http.Redirect(w, r, redirect, http.StatusFound)
	}
}

func (m *Module) authenticate(d *core.Deps, identifier, password, ip string) (*core.User, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" || password == "" {
		return nil, "", core.Errf("missing_credentials", "请输入账号和密码")
	}
	// 登录爆破防护：同 IP + 同账号，10 次/5 分钟。
	if ok, _, _ := core.Allow(d.DB, "login:"+ip+":"+strings.ToLower(identifier), 10, 300, d.Unix()); !ok {
		return nil, "", core.NewErr(http.StatusTooManyRequests, "rate_limited", "尝试过于频繁，请稍后再试")
	}

	var (
		id    int64
		email string
		role  string
		bal   int64
		hash  string
	)
	err := d.DB.QueryRow(`SELECT id, email, role, balance, pass_hash FROM users
		WHERE email = ? OR username = ?`, strings.ToLower(identifier), identifier).
		Scan(&id, &email, &role, &bal, &hash)
	if err != nil {
		return nil, "", core.NewErr(http.StatusUnauthorized, "bad_credentials", "账号或密码错误")
	}
	if !d.Auth.CheckPassword(hash, password) {
		core.Audit(d.DB, 0, "login_failed", identifier, "", ip, d.Unix())
		return nil, "", core.NewErr(http.StatusUnauthorized, "bad_credentials", "账号或密码错误")
	}
	u := &core.User{ID: id, Email: email, Role: role, Balance: bal}
	token, err := d.Auth.Issue(u)
	if err != nil {
		return nil, "", err
	}
	core.Audit(d.DB, id, "login", email, "", ip, d.Unix())
	return u, token, nil
}

func (m *Module) logout(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.Auth.ClearCookie(w)
		core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ---------------------------------------------------------------------------
// 用户信息 / 用户中心
// ---------------------------------------------------------------------------

func (m *Module) publicUser(d *core.Deps, id int64) map[string]any {
	out := map[string]any{}
	var (
		email, username, role, invite string
		bal, vip, granted             int64
		created                       int64
	)
	err := d.DB.QueryRow(`SELECT email, username, role, balance, invite_code, vip_until, signup_granted, created_at
		FROM users WHERE id = ?`, id).
		Scan(&email, &username, &role, &bal, &invite, &vip, &granted, &created)
	if err != nil {
		return out
	}
	out["id"] = id
	out["email"] = email
	out["username"] = username
	out["role"] = role
	out["balance"] = bal
	out["invite_code"] = invite
	out["vip_until"] = vip
	out["signup_granted"] = granted
	out["created_at"] = created
	return out
}

func (m *Module) me(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		core.WriteJSON(w, http.StatusOK, m.publicUser(d, u.ID))
	}
}

func (m *Module) overview(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		out := m.publicUser(d, u.ID)

		var total, done, failed, subs, pending int64
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE user_id = ?`, u.ID).Scan(&total)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE user_id = ? AND status = 'done'`, u.ID).Scan(&done)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM tasks WHERE user_id = ? AND status = 'failed'`, u.ID).Scan(&failed)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM subscriptions WHERE user_id = ? AND status = 'active'`, u.ID).Scan(&subs)
		_ = d.DB.QueryRow(`SELECT COUNT(1) FROM payments WHERE user_id = ? AND status = 'pending'`, u.ID).Scan(&pending)

		txns := []map[string]any{}
		rows, err := d.DB.Query(`SELECT delta, balance_after, reason, created_at FROM txns
			WHERE user_id = ? ORDER BY id DESC LIMIT 5`, u.ID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var delta, after, at int64
				var reason string
				if rows.Scan(&delta, &after, &reason, &at) == nil {
					txns = append(txns, map[string]any{"delta": delta, "balance_after": after, "reason": reason, "created_at": at})
				}
			}
		}

		core.WriteJSON(w, http.StatusOK, map[string]any{
			"user":      out,
			"tasks":     map[string]any{"total": total, "done": done, "failed": failed},
			"subs":      subs,
			"payments":  map[string]any{"pending": pending},
			"txns":      txns,
			"menus":     d.Cfg.Section("ui")["menus"],
			"promotion": d.Cfg.Section("ui")["promotion"],
		})
	}
}

// ---------------------------------------------------------------------------
// SSO
// ---------------------------------------------------------------------------

func (m *Module) sso(d *core.Deps) http.HandlerFunc {
	return d.Auth.OptionalUser(func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("redirect")
		if target == "" {
			target = "/"
		}
		if !m.redirectAllowed(d, target) {
			core.Fail(w, core.NewErr(http.StatusBadRequest, "bad_redirect", "redirect 不在白名单内"))
			return
		}
		u := core.UserFrom(r)
		if u == nil {
			back := "/auth/login?redirect=" + url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		code := core.RandomToken(32)
		now := d.Unix()
		if _, err := d.DB.Exec(`INSERT INTO sso_codes(code, user_id, redirect, expires_at, used) VALUES(?,?,?,?,0)`,
			code, u.ID, target, now+60); err != nil {
			core.Fail(w, err)
			return
		}
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		http.Redirect(w, r, target+sep+"code="+code, http.StatusFound)
	})
}

func (m *Module) ssoExchange(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code string `json:"code"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		now := d.Unix()
		var (
			userID   int64
			redirect string
			expires  int64
			used     int
		)
		err := d.DB.QueryRow(`SELECT user_id, redirect, expires_at, used FROM sso_codes WHERE code = ?`, req.Code).
			Scan(&userID, &redirect, &expires, &used)
		if err != nil || used == 1 || expires < now {
			core.Fail(w, core.NewErr(http.StatusBadRequest, "bad_code", "code 无效或已过期"))
			return
		}
		if _, err := d.DB.Exec(`UPDATE sso_codes SET used = 1 WHERE code = ?`, req.Code); err != nil {
			core.Fail(w, err)
			return
		}
		var u core.User
		if err := d.DB.QueryRow(`SELECT id, email, role, balance FROM users WHERE id = ?`, userID).
			Scan(&u.ID, &u.Email, &u.Role, &u.Balance); err != nil {
			core.Fail(w, err)
			return
		}
		token, err := d.Auth.Issue(&u)
		if err != nil {
			core.Fail(w, err)
			return
		}
		d.Auth.SetCookie(w, token)
		core.WriteJSON(w, http.StatusOK, map[string]any{"token": token, "user_id": u.ID, "redirect": redirect})
	}
}

// redirectAllowed 校验回跳地址：相对路径放行；绝对地址必须命中站点 base_url 或配置白名单。
func (m *Module) redirectAllowed(d *core.Deps, target string) bool {
	if strings.HasPrefix(target, "//") || strings.ContainsAny(target, "\\") || strings.Contains(target, "@") {
		return false
	}
	if strings.HasPrefix(target, "/") {
		return !strings.Contains(target, "..")
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	origin := u.Scheme + "://" + u.Host
	for _, v := range stringList(d.Cfg.Section("auth")["redirect_allow"]) {
		if strings.TrimRight(v, "/") == origin {
			return true
		}
	}
	rows, err := d.DB.Query(`SELECT base_url FROM sites WHERE revoked = 0 AND base_url <> ''`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var base string
		if rows.Scan(&base) != nil {
			continue
		}
		if strings.TrimRight(base, "/") == origin {
			return true
		}
	}
	return false
}

func safeRedirect(d *core.Deps, m *Module, v string) string {
	if v == "" || !m.redirectAllowed(d, v) {
		return "/"
	}
	return v
}

// ---------------------------------------------------------------------------
// 托管认证页
// ---------------------------------------------------------------------------

type pageData struct {
	Brand     string
	Primary   string
	Mode      string
	Redirect  string
	Error     string
	InviteReq bool
}

var pageTpl = template.Must(template.New("auth").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Brand}}</title>
<style>
:root{--p:{{.Primary}}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#e9edf4;color:#2a3244;
font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","Microsoft YaHei",sans-serif}
.card{width:min(400px,92vw);background:#e9edf4;border:1px solid rgba(255,255,255,.65);border-radius:20px;
padding:30px;box-shadow:10px 10px 26px rgba(163,177,198,.5),-10px -10px 26px rgba(255,255,255,.9)}
h1{margin:0 0 4px;font-size:21px;font-weight:750;letter-spacing:-.2px}
.sub{margin:0 0 22px;color:#707d93;font-size:13px}
label{display:block;margin-bottom:14px;font-size:13px;color:#5b6779}
input{width:100%;margin-top:7px;padding:12px 14px;border-radius:12px;border:1px solid transparent;
background:#e9edf4;color:#2a3244;font-size:14px;outline:none;
box-shadow:inset 2px 2px 5px rgba(163,177,198,.6),inset -2px -2px 5px rgba(255,255,255,.95)}
input:focus{box-shadow:inset 2px 2px 5px rgba(163,177,198,.6),inset -2px -2px 5px rgba(255,255,255,.95),0 0 0 3px rgba(74,124,247,.18)}
button{width:100%;margin-top:8px;padding:13px;border:1px solid rgba(255,255,255,.3);border-radius:13px;
background:linear-gradient(145deg,#6b95ff,#3f6fe0);color:#fff;font-size:15px;font-weight:650;cursor:pointer;
box-shadow:0 6px 14px rgba(74,124,247,.35),3px 3px 7px rgba(163,177,198,.55),-3px -3px 7px rgba(255,255,255,.9)}
button:hover{filter:brightness(1.04)}
button:active{box-shadow:inset 3px 3px 7px rgba(40,70,160,.45)}
.err{margin:0 0 14px;padding:11px 14px;border-radius:12px;background:#e9edf4;color:#b03b3b;font-size:13px;
box-shadow:inset 2px 2px 5px rgba(163,177,198,.6),inset -2px -2px 5px rgba(255,255,255,.95)}
.hint{margin:18px 0 0;font-size:13px;color:#707d93;text-align:center}
a{color:var(--p);text-decoration:none;font-weight:600}
.made{margin:22px 0 0;padding-top:16px;text-align:center;font-size:11.5px;color:#98a4b8;letter-spacing:.3px;
border-top:1px solid rgba(163,177,198,.3)}
.made b{font-weight:650;color:#707d93}
</style></head><body>
<div class="card">
  <h1>{{.Brand}}</h1>
  <p class="sub">{{if eq .Mode "login"}}登录你的账号{{else}}创建新账号{{end}}</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  {{if eq .Mode "login"}}
  <form method="post" action="/auth/login">
    <input type="hidden" name="redirect" value="{{.Redirect}}">
    <label>邮箱或用户名<input name="identifier" required autocomplete="username" autofocus></label>
    <label>密码<input type="password" name="password" required autocomplete="current-password"></label>
    <button type="submit">登 录</button>
    <p class="hint">还没有账号？<a href="/auth/register?redirect={{.Redirect}}">立即注册</a></p>
  </form>
  {{else}}
  <form method="post" action="/auth/register">
    <input type="hidden" name="redirect" value="{{.Redirect}}">
    <label>邮箱<input name="email" type="email" required autocomplete="email" autofocus></label>
    <label>用户名<input name="username" required pattern="[A-Za-z0-9_]{3,24}" autocomplete="username"></label>
    <label>密码<input type="password" name="password" required minlength="8" autocomplete="new-password"></label>
    {{if .InviteReq}}<label>邀请码<input name="invite_code" required></label>{{end}}
    <button type="submit">注 册</button>
    <p class="hint">已有账号？<a href="/auth/login?redirect={{.Redirect}}">去登录</a></p>
  </form>
  {{end}}
  <p class="made">Powered by <b>万界零方</b></p>
</div></body></html>`))

func (m *Module) page(d *core.Deps, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		brand := d.Cfg.Section("brand")
		redirect := r.URL.Query().Get("redirect")
		if redirect == "" {
			redirect = "/"
		}
		if !m.redirectAllowed(d, redirect) {
			redirect = "/"
		}
		data := pageData{
			Brand:     str(brand["name"], "MC AI"),
			Primary:   str(brand["primary"], "#3b82f6"),
			Mode:      mode,
			Redirect:  redirect,
			Error:     r.URL.Query().Get("err"),
			InviteReq: d.Cfg.Bool("auth", "invite_required", false),
		}
		var buf bytes.Buffer
		if err := pageTpl.Execute(&buf, data); err != nil {
			core.Fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(buf.Bytes())
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func str(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func stringList(v any) []string {
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
