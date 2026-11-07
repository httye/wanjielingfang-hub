package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Claims 是 JWT 载荷。
type Claims struct {
	Sub  int64  `json:"sub"`
	Role string `json:"role"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
	Iss  string `json:"iss"`
}

// Auth 负责密码、JWT、Cookie、站点/Worker 身份校验与签名链接。
type Auth struct {
	DB           *sql.DB
	Secret       []byte
	CookieName   string
	CookieDomain string
	CookieSecure bool
	Issuer       string
	TTL          time.Duration
	Now          func() time.Time
}

func (a *Auth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// 密码
// ---------------------------------------------------------------------------

// HashPassword 用 bcrypt 生成哈希。
func (a *Auth) HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	return string(h), err
}

// CheckPassword 校验密码。
func (a *Auth) CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// ---------------------------------------------------------------------------
// JWT（HS256，显式校验 alg，防 alg:none）
// ---------------------------------------------------------------------------

var errBadToken = errors.New("token 无效或已过期")

// Issue 给用户签发 token。
func (a *Auth) Issue(u *User) (string, error) {
	now := a.now().Unix()
	claims := Claims{Sub: u.ID, Role: u.Role, Iat: now, Exp: now + int64(a.TTL.Seconds()), Iss: a.Issuer}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return a.signJWT(`{"alg":"HS256","typ":"JWT"}`, string(payload)), nil
}

func (a *Auth) signJWT(header, payload string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, a.Secret)
	mac.Write([]byte(h + "." + p))
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Parse 校验并解析 token。
func (a *Auth) Parse(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errBadToken
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errBadToken
	}
	var head struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(hb, &head); err != nil || head.Alg != "HS256" {
		return nil, errBadToken
	}
	mac := hmac.New(sha256.New, a.Secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil, errBadToken
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errBadToken
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, errBadToken
	}
	if c.Exp <= a.now().Unix() {
		return nil, errBadToken
	}
	if a.Issuer != "" && c.Iss != a.Issuer {
		return nil, errBadToken
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// Cookie
// ---------------------------------------------------------------------------

// SetCookie 下发登录 Cookie。
func (a *Auth) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.CookieName,
		Value:    token,
		Path:     "/",
		Domain:   a.CookieDomain,
		HttpOnly: true,
		Secure:   a.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(a.TTL.Seconds()),
	})
}

// ClearCookie 清除登录 Cookie。
func (a *Auth) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.CookieName,
		Value:    "",
		Path:     "/",
		Domain:   a.CookieDomain,
		HttpOnly: true,
		Secure:   a.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// TokenFrom 从 Authorization 头或 Cookie 中取 token。
func (a *Auth) TokenFrom(r *http.Request) string {
	if t := Bearer(r); t != "" {
		return t
	}
	if c, err := r.Cookie(a.CookieName); err == nil {
		return c.Value
	}
	return ""
}

// ---------------------------------------------------------------------------
// 身份中间件
// ---------------------------------------------------------------------------

// RequireUser 强制登录，并把用户塞进上下文。
func (a *Auth) RequireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := a.resolve(r)
		if err != nil {
			Fail(w, err)
			return
		}
		next(w, withUser(r, u))
	}
}

// OptionalUser 有 token 就解析，没有也放行。
func (a *Auth) OptionalUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, err := a.resolve(r); err == nil {
			r = withUser(r, u)
		}
		next(w, r)
	}
}

// RequireAdmin 要求 role=admin。
func (a *Auth) RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return a.RequireUser(func(w http.ResponseWriter, r *http.Request) {
		if !IsAdmin(UserFrom(r)) {
			Fail(w, NewErr(http.StatusForbidden, "forbidden", "需要管理员权限"))
			return
		}
		next(w, r)
	})
}

func (a *Auth) resolve(r *http.Request) (*User, error) {
	tok := a.TokenFrom(r)
	if tok == "" {
		return nil, NewErr(http.StatusUnauthorized, "unauthorized", "未登录")
	}
	cl, err := a.Parse(tok)
	if err != nil {
		return nil, NewErr(http.StatusUnauthorized, "unauthorized", err.Error())
	}
	var u User
	err = a.DB.QueryRow(`SELECT id, email, role, balance FROM users WHERE id = ?`, cl.Sub).
		Scan(&u.ID, &u.Email, &u.Role, &u.Balance)
	if err != nil {
		return nil, NewErr(http.StatusUnauthorized, "unauthorized", "用户不存在")
	}
	return &u, nil
}

// MustResolve 供需要“站点 + 用户”双身份的接口使用（拿不到用户则返回错误）。
func (a *Auth) MustResolve(r *http.Request) (*User, error) { return a.resolve(r) }

// RequireSite 校验 X-Site-Key，并把站点塞进上下文。
func (a *Auth) RequireSite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Site-Key")
		if key == "" {
			Fail(w, NewErr(http.StatusUnauthorized, "site_key_required", "缺少 X-Site-Key"))
			return
		}
		var s SiteRef
		var revoked int
		err := a.DB.QueryRow(`SELECT id, user_id, site_key, name, revoked FROM sites WHERE site_key = ?`, key).
			Scan(&s.ID, &s.UserID, &s.Key, &s.Name, &revoked)
		if err != nil {
			Fail(w, NewErr(http.StatusUnauthorized, "bad_site_key", "站点 key 无效"))
			return
		}
		if revoked == 1 {
			Fail(w, NewErr(http.StatusForbidden, "site_revoked", "站点已吊销"))
			return
		}
		next(w, withSite(r, &s))
	}
}

// RequireWorker 校验 X-Worker-Key。
func (a *Auth) RequireWorker(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Worker-Key")
		if key == "" {
			Fail(w, NewErr(http.StatusUnauthorized, "worker_key_required", "缺少 X-Worker-Key"))
			return
		}
		var id int64
		if err := a.DB.QueryRow(`SELECT id FROM workers WHERE worker_key = ?`, key).Scan(&id); err != nil {
			Fail(w, NewErr(http.StatusUnauthorized, "bad_worker_key", "worker key 无效"))
			return
		}
		next(w, r.WithContext(contextWithWorker(r.Context(), id)))
	}
}

// ---------------------------------------------------------------------------
// 签名链接（下载 / 回调）
// ---------------------------------------------------------------------------

// Sign 对 "path|exp" 做 HMAC。
func (a *Auth) Sign(path string, exp int64) string {
	mac := hmac.New(sha256.New, a.Secret)
	mac.Write([]byte(path + "|" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySign 校验签名。
func (a *Auth) VerifySign(path string, exp int64, sig string) bool {
	if a.now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(a.Sign(path, exp)), []byte(sig))
}

// SignedDownload 生成带签名的下载地址。
func (a *Auth) SignedDownload(taskID int64, ttl time.Duration) string {
	exp := a.now().Add(ttl).Unix()
	path := "/api/v1/tasks/" + strconv.FormatInt(taskID, 10) + "/download"
	return path + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + a.Sign(path, exp)
}
