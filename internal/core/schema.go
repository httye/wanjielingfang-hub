package core

import "database/sql"

// BaseSchema 建所有模块共享的表（与业务无关的基础设施）。
func BaseSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS config (
			section    TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			version    INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			site_id      INTEGER NOT NULL DEFAULT 0,
			type         TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			status       INTEGER NOT NULL DEFAULT 0,
			attempts     INTEGER NOT NULL DEFAULT 0,
			next_at      INTEGER NOT NULL,
			created_at   INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_due ON events(status, next_at)`,
		`CREATE TABLE IF NOT EXISTS rate_counters (
			key          TEXT PRIMARY KEY,
			window_start INTEGER NOT NULL,
			count        INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_idem (
			key        TEXT PRIMARY KEY,
			task_id    INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			actor_id   INTEGER NOT NULL DEFAULT 0,
			action     TEXT NOT NULL,
			target     TEXT NOT NULL DEFAULT '',
			detail     TEXT NOT NULL DEFAULT '',
			ip         TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS site_revenue (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			site_id    INTEGER NOT NULL,
			month      TEXT NOT NULL,
			amount     INTEGER NOT NULL DEFAULT 0,
			source     TEXT NOT NULL DEFAULT '',
			status     TEXT NOT NULL DEFAULT 'pending',
			created_at INTEGER NOT NULL,
			UNIQUE(site_id, month, source)
		)`,
		`CREATE TABLE IF NOT EXISTS price_rules (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			action     TEXT NOT NULL,
			site_id    INTEGER NOT NULL DEFAULT 0,
			platform   TEXT NOT NULL DEFAULT '',
			amount     INTEGER NOT NULL,
			currency   TEXT NOT NULL DEFAULT 'CNY',
			valid_from INTEGER NOT NULL DEFAULT 0,
			valid_to   INTEGER NOT NULL DEFAULT 0,
			priority   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_price_action ON price_rules(action, site_id, priority DESC)`,
		`CREATE TABLE IF NOT EXISTS txns (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id       INTEGER NOT NULL,
			delta         INTEGER NOT NULL,
			balance_after INTEGER NOT NULL,
			reason        TEXT NOT NULL,
			ref_type      TEXT NOT NULL DEFAULT '',
			ref_id        INTEGER NOT NULL DEFAULT 0,
			site_id       INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_txns_user ON txns(user_id, id DESC)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// Audit 写一条审计日志（失败不影响业务）。
func Audit(db *sql.DB, actorID int64, action, target, detail, ip string, now int64) {
	_, _ = db.Exec(`INSERT INTO audit_logs(actor_id, action, target, detail, ip, created_at)
		VALUES(?,?,?,?,?,?)`, actorID, action, target, detail, ip, now)
}
