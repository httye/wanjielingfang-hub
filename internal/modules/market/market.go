// Package market 负责成品插件市场的发布、购买与分成。
package market

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mcai/hub/internal/core"
)

const ddl = `
CREATE TABLE IF NOT EXISTS products (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id     INTEGER NOT NULL,
	seller_id   INTEGER NOT NULL,
	title       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	price       INTEGER NOT NULL DEFAULT 0,
	platform    TEXT NOT NULL DEFAULT '',
	mc_version  TEXT NOT NULL DEFAULT '',
	downloads   INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL DEFAULT 'on',
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_products_status ON products(status, id DESC);
CREATE INDEX IF NOT EXISTS idx_products_seller ON products(seller_id, id DESC);
CREATE TABLE IF NOT EXISTS orders (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	product_id    INTEGER NOT NULL,
	buyer_id      INTEGER NOT NULL,
	price         INTEGER NOT NULL,
	commission    INTEGER NOT NULL DEFAULT 0,
	seller_income INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_unique ON orders(product_id, buyer_id);
`

// Module 是市场模块。
type Module struct {
	DataDir string
}

// New 构造。
func New(dataDir string) *Module { return &Module{DataDir: dataDir} }

// Name 模块名。
func (m *Module) Name() string { return "market" }

// Migrate 建表。
func (m *Module) Migrate(db *sql.DB) error { return core.ApplyMigration(db, m.Name(), "v1", ddl) }

// Defaults 无额外配置（抽成走 billing.commission_rate）。
func (m *Module) Defaults() map[string]map[string]any { return nil }

// Routes 挂载路由。
func (m *Module) Routes(mux *http.ServeMux, d *core.Deps) {
	mux.HandleFunc("POST /api/market/publish", d.Auth.RequireUser(m.publish(d)))
	mux.HandleFunc("GET /api/market", m.list(d))
	mux.HandleFunc("GET /api/market/{id}", m.detail(d))
	mux.HandleFunc("POST /api/market/buy", d.Auth.RequireUser(m.buy(d)))
	mux.HandleFunc("GET /api/market/download", d.Auth.RequireUser(m.download(d)))
	mux.HandleFunc("GET /api/market/mine", d.Auth.RequireUser(m.mine(d)))
}

func (m *Module) publish(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TaskID      int64  `json:"task_id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Price       int64  `json:"price"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if req.Title == "" {
			core.Fail(w, core.Errf("bad_title", "请填写标题"))
			return
		}
		if req.Price < 0 {
			core.Fail(w, core.Errf("bad_price", "价格不能为负"))
			return
		}
		u := core.UserFrom(r)
		var (
			owner         int64
			platform, ver string
			st            string
		)
		err := d.DB.QueryRow(`SELECT user_id, platform, mc_version, status FROM tasks WHERE id = ?`, req.TaskID).
			Scan(&owner, &platform, &ver, &st)
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "任务不存在"))
			return
		}
		if owner != u.ID && !core.IsAdmin(u) {
			core.Fail(w, core.NewErr(http.StatusForbidden, "forbidden", "只能发布自己的任务"))
			return
		}
		if st != "done" {
			core.Fail(w, core.NewErr(http.StatusConflict, "not_done", "任务未完成，无法发布"))
			return
		}
		var dup int64
		if err := d.DB.QueryRow(`SELECT COUNT(1) FROM products WHERE task_id = ? AND seller_id = ?`,
			req.TaskID, owner).Scan(&dup); err == nil && dup > 0 {
			core.Fail(w, core.NewErr(http.StatusConflict, "already_published", "该任务已发布过，不能重复发布"))
			return
		}
		now := d.Unix()
		res, err := d.DB.Exec(`INSERT INTO products(task_id, seller_id, title, description, price, platform, mc_version, downloads, status, created_at)
			VALUES(?,?,?,?,?,?,?,0,'on',?)`, req.TaskID, owner, req.Title, req.Description, req.Price, platform, ver, now)
		if err != nil {
			core.Fail(w, err)
			return
		}
		id, _ := res.LastInsertId()
		core.WriteJSON(w, http.StatusCreated, map[string]any{"product_id": id, "status": "on"})
	}
}

func (m *Module) list(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.DB.Query(`SELECT id, title, description, price, platform, mc_version, downloads, created_at
			FROM products WHERE status = 'on' ORDER BY id DESC LIMIT 100`)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, price, downloads, created int64
				title, desc, platform, ver    string
			)
			if rows.Scan(&id, &title, &desc, &price, &platform, &ver, &downloads, &created) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "title": title, "description": desc, "price": price,
				"platform": platform, "mc_version": ver, "downloads": downloads, "created_at": created,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (m *Module) detail(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := core.PathInt(r, "id")
		if err != nil {
			core.Fail(w, err)
			return
		}
		var (
			taskID, sellerID, price, downloads, created int64
			title, desc, platform, ver, status          string
		)
		err = d.DB.QueryRow(`SELECT task_id, seller_id, title, description, price, platform, mc_version, downloads, status, created_at
			FROM products WHERE id = ?`, id).
			Scan(&taskID, &sellerID, &title, &desc, &price, &platform, &ver, &downloads, &status, &created)
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "商品不存在"))
			return
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{
			"id": id, "title": title, "description": desc, "price": price, "platform": platform,
			"mc_version": ver, "downloads": downloads, "status": status, "created_at": created,
		})
	}
}

func (m *Module) buy(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ProductID int64 `json:"product_id"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		u := core.UserFrom(r)
		now := d.Unix()

		var (
			productID, taskID, sellerID, price int64
			status                             string
		)
		err := d.DB.QueryRow(`SELECT id, task_id, seller_id, price, status FROM products WHERE id = ?`, req.ProductID).
			Scan(&productID, &taskID, &sellerID, &price, &status)
		if err != nil || status != "on" {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "商品不可购买"))
			return
		}
		if sellerID == u.ID {
			core.Fail(w, core.NewErr(http.StatusConflict, "own_product", "不能购买自己发布的作品"))
			return
		}
		var existing int64
		if err := d.DB.QueryRow(`SELECT id FROM orders WHERE product_id = ? AND buyer_id = ?`, productID, u.ID).
			Scan(&existing); err == nil {
			core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "already": true, "order_id": existing})
			return
		}

		rate := d.Cfg.Int("billing", "commission_rate", 10)
		commission := price * rate / 100
		sellerIncome := price - commission

		err = core.Tx(r.Context(), d.DB, func(tx *sql.Tx) error {
			if price > 0 {
				if _, err := core.AddBalance(tx, u.ID, -price, "market_buy", "product", productID, 0, now); err != nil {
					return err
				}
				if sellerIncome > 0 {
					if _, err := core.AddBalance(tx, sellerID, sellerIncome, "market_sell", "product", productID, 0, now); err != nil {
						return err
					}
				}
			}
			if _, err := tx.Exec(`INSERT INTO orders(product_id, buyer_id, price, commission, seller_income, created_at)
				VALUES(?,?,?,?,?,?)`, productID, u.ID, price, commission, sellerIncome, now); err != nil {
				if isUnique(err) {
					return core.NewErr(http.StatusConflict, "already_bought", "该商品已购买")
				}
				return err
			}
			_, err := tx.Exec(`UPDATE products SET downloads = downloads + 1 WHERE id = ?`, productID)
			return err
		})
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, map[string]any{
			"ok": true, "product_id": productID, "price": price, "commission": commission,
		})
	}
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), "UNIQUE")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func (m *Module) mine(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := core.UserFrom(r)
		rows, err := d.DB.Query(`SELECT p.id, p.title, p.price, p.downloads,
			COALESCE((SELECT SUM(seller_income) FROM orders o WHERE o.product_id = p.id), 0) AS income
			FROM products p WHERE p.seller_id = ? ORDER BY p.id DESC`, u.ID)
		if err != nil {
			core.Fail(w, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id, price, downloads, income int64
			var title string
			if rows.Scan(&id, &title, &price, &downloads, &income) != nil {
				continue
			}
			items = append(items, map[string]any{
				"id": id, "title": title, "price": price, "downloads": downloads, "income": income,
			})
		}
		core.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func modTime(path string) time.Time {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func (m *Module) download(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		productID := core.QueryInt(r, "product_id", 0)
		u := core.UserFrom(r)
		if productID <= 0 {
			core.Fail(w, core.Errf("bad_product", "缺少 product_id"))
			return
		}
		var taskID, sellerID, price int64
		if err := d.DB.QueryRow(`SELECT task_id, seller_id, price FROM products WHERE id = ?`, productID).
			Scan(&taskID, &sellerID, &price); err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "not_found", "商品不存在"))
			return
		}
		if price > 0 && sellerID != u.ID {
			var n int64
			_ = d.DB.QueryRow(`SELECT COUNT(1) FROM orders WHERE product_id = ? AND buyer_id = ?`, productID, u.ID).Scan(&n)
			if n == 0 {
				core.Fail(w, core.NewErr(http.StatusPaymentRequired, "not_purchased", "尚未购买该商品"))
				return
			}
		}
		var status, jarPath string
		if err := d.DB.QueryRow(`SELECT status, jar_path FROM tasks WHERE id = ?`, taskID).Scan(&status, &jarPath); err != nil || status != "done" {
			core.Fail(w, core.NewErr(http.StatusConflict, "not_ready", "产物不可用"))
			return
		}
		abs := filepath.Join(m.DataDir, filepath.Base(jarPath))
		f, err := os.Open(abs)
		if err != nil {
			core.Fail(w, core.NewErr(http.StatusNotFound, "artifact_missing", "产物文件缺失"))
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/java-archive")
		w.Header().Set("Content-Disposition", `attachment; filename="market-`+filepath.Base(jarPath)+`"`)
		http.ServeContent(w, r, filepath.Base(abs), modTime(abs), f)
	}
}
