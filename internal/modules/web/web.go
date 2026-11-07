// Package web 提供 hub 自带的两个页面：用户中心(/console)与管理后台(/admin)。
// 纯静态资源 + 原生 JS，直接调 hub 的 JSON API，不引入构建链。
package web

import (
	"database/sql"
	"embed"
	"net/http"
	"path"
	"strings"

	"github.com/mcai/hub/internal/core"
)

//go:embed console.html admin.html assets
var content embed.FS

// Module 是页面模块。
type Module struct{}

// New 构造。
func New() *Module { return &Module{} }

// Name 模块名。
func (m *Module) Name() string { return "web" }

// Migrate 无自有表。
func (m *Module) Migrate(db *sql.DB) error { return nil }

// Defaults 无配置。
func (m *Module) Defaults() map[string]map[string]any { return nil }

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console", http.StatusFound)
	})
	mux.HandleFunc("GET /console", m.page(d, "console.html", "/console", false))
	mux.HandleFunc("GET /admin", m.page(d, "admin.html", "/admin", true))
	mux.HandleFunc("GET /assets/{name}", m.asset())
}

// page 输出页面；未登录跳托管登录页，/admin 额外要求 admin 角色。
func (m *Module) page(d *core.Deps, file, self string, needAdmin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := d.Auth.TokenFrom(r)
		if tok == "" {
			http.Redirect(w, r, "/auth/login?redirect="+self, http.StatusFound)
			return
		}
		claims, err := d.Auth.Parse(tok)
		if err != nil {
			http.Redirect(w, r, "/auth/login?redirect="+self, http.StatusFound)
			return
		}
		if needAdmin && claims.Role != "admin" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>403</title><style>body{background:#0b1020;color:#e8ecf5;font:15px/1.7 -apple-system,"Microsoft YaHei",sans-serif;
display:grid;place-items:center;height:100vh;margin:0}a{color:#3b82f6}</style></head><body><div style="text-align:center">
<h1 style="font-size:20px">403 需要管理员权限</h1><p style="color:#8b97b3">当前账号是普通用户。</p>
<p><a href="/console">去用户中心</a></p></div></body></html>`))
			return
		}
		data, rerr := content.ReadFile(file)
		if rerr != nil {
			core.Fail(w, rerr)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		_, _ = w.Write(data)
	}
}

// asset 输出静态资源，只允许 assets 目录下的单层文件名。
func (m *Module) asset() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
			http.NotFound(w, r)
			return
		}
		data, err := content.ReadFile("assets/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ct := "application/octet-stream"
		switch path.Ext(name) {
		case ".css":
			ct = "text/css; charset=utf-8"
		case ".js":
			ct = "application/javascript; charset=utf-8"
		case ".svg":
			ct = "image/svg+xml"
		}
		w.Header().Set("Content-Type", ct)
		// 页面会随版本更新，静态资源不做长缓存，避免改动后浏览器拿到旧脚本。
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		_, _ = w.Write(data)
	}
}
