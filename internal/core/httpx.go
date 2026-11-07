package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 统一错误
// ---------------------------------------------------------------------------

// APIError 是返回给客户端的结构化错误。
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Msg }

// NewErr 构造带状态码的错误。
func NewErr(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Msg: msg}
}

// Errf 构造 400 错误。
func Errf(code, format string, a ...any) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: code, Msg: sprintf(format, a...)}
}

// Fail 写出错误响应；未知错误统一 500。
func Fail(w http.ResponseWriter, err error) {
	var ae *APIError
	if errors.As(err, &ae) {
		WriteJSON(w, ae.Status, map[string]any{"error": ae.Code, "message": ae.Msg})
		return
	}
	WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal", "message": err.Error()})
}

// WriteJSON 输出 JSON。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// ReadJSON 读取请求体（上限 8MB）。
func ReadJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return NewErr(http.StatusBadRequest, "bad_body", "读取请求体失败")
	}
	if len(body) == 0 {
		return NewErr(http.StatusBadRequest, "bad_body", "请求体为空")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return NewErr(http.StatusBadRequest, "bad_json", "JSON 解析失败: "+err.Error())
	}
	return nil
}

// QueryInt 读查询参数整数。
func QueryInt(r *http.Request, key string, def int64) int64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// PathInt 读路径参数整数（Go 1.22 pattern {id}）。
func PathInt(r *http.Request, key string) (int64, error) {
	v := r.PathValue(key)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, NewErr(http.StatusBadRequest, "bad_id", "非法的 "+key+" 参数")
	}
	return n, nil
}

// ClientIP 取真实客户端 IP。
// 只有来自回环/私网（也就是本机反代）的请求才采信转发头——否则公网请求伪造
// X-Forwarded-For 就能换掉限流键，注册/登录限流会被轻易绕过。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedProxy(host) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	return host
}

func isTrustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// Bearer 取 Authorization 头中的 token。
func Bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ---------------------------------------------------------------------------
// 上下文
// ---------------------------------------------------------------------------

type ctxKey int

const (
	ctxKeyUser ctxKey = iota + 1
	ctxKeySite
	ctxKeyReqID
	ctxKeyWorker
)

// User 是登录态用户的最小投影。
type User struct {
	ID      int64  `json:"id"`
	Email   string `json:"email"`
	Role    string `json:"role"`
	Balance int64  `json:"balance"`
}

func IsAdmin(u *User) bool { return u != nil && u.Role == "admin" }

func withUser(r *http.Request, u *User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeyUser, u))
}

// UserFrom 取请求里的用户。
func UserFrom(r *http.Request) *User {
	if u, ok := r.Context().Value(ctxKeyUser).(*User); ok {
		return u
	}
	return nil
}

func withSite(r *http.Request, s *SiteRef) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeySite, s))
}

// SiteRef 是校验通过的站点身份。
type SiteRef struct {
	ID     int64
	UserID int64
	Key    string
	Name   string
}

// SiteFrom 取请求里的站点。
func SiteFrom(r *http.Request) *SiteRef {
	if s, ok := r.Context().Value(ctxKeySite).(*SiteRef); ok {
		return s
	}
	return nil
}

func contextWithWorker(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, ctxKeyWorker, id)
}

// WorkerID 取请求里的 worker id（0 表示没有）。
func WorkerID(r *http.Request) int64 {
	if v, ok := r.Context().Value(ctxKeyWorker).(int64); ok {
		return v
	}
	return 0
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

type middlewares struct {
	log      Logger
	origin   map[string]bool
	wildcard bool
}

// Logger 是 core 需要的日志接口。
type Logger interface {
	Printf(format string, a ...any)
}

// Middleware 返回带 CORS / 恢复 / 日志的中间件链。
func Middleware(log Logger, allowedOrigins string) func(http.Handler) http.Handler {
	m := &middlewares{log: log, origin: map[string]bool{}}
	for _, o := range strings.Split(allowedOrigins, ",") {
		o = strings.TrimSpace(o)
		if o == "*" {
			m.wildcard = true
			continue
		}
		if o != "" {
			m.origin[o] = true
		}
	}
	return m.wrap
}

func (m *middlewares) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if o := r.Header.Get("Origin"); o != "" {
			if m.wildcard || m.origin[o] {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Site-Key, X-Worker-Key, Idempotency-Key, If-None-Match")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Expose-Headers", "ETag, X-Config-Stale")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		rw := &respWriter{ResponseWriter: w}

		defer func() {
			if rec := recover(); rec != nil {
				m.log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, rec)
				if !rw.wrote {
					WriteJSON(rw, http.StatusInternalServerError, map[string]any{"error": "panic", "message": "internal error"})
				}
			}
			if d := time.Since(start); d > 800*time.Millisecond {
				m.log.Printf("SLOW %s %s %s", r.Method, r.URL.Path, d)
			}
		}()

		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(rw, r)
	})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// respWriter 记录是否已经写过响应头，避免 panic 恢复分支二次 WriteHeader
// 产生 "superfluous WriteHeader" 与半截 JSON。同时把 Flush 透传出去，保证 SSE 可用。
type respWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *respWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *respWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// sprintf 只用于错误文本拼装，避免到处 import fmt。
func sprintf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}
