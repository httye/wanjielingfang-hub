package core

import (
	"database/sql"
)

// ErrInsufficient 余额不足。
var ErrInsufficient = NewErr(402, "insufficient_balance", "余额不足，请先充值")

// AddBalance 在事务内改余额并写流水，返回新余额。
// 用单条条件 UPDATE 完成「读-改-写」，避免并发事务先读后写升级锁导致的 SQLITE_BUSY_SNAPSHOT。
// 余额不足时返回 ErrInsufficient，调用方应回滚事务。
func AddBalance(tx *sql.Tx, userID, delta int64, reason, refType string, refID, siteID, now int64) (int64, error) {
	var next int64
	err := tx.QueryRow(`UPDATE users SET balance = balance + ?, updated_at = ?
		WHERE id = ? AND balance + ? >= 0
		RETURNING balance`, delta, now, userID, delta).Scan(&next)
	if err == sql.ErrNoRows {
		var exists int
		if e := tx.QueryRow(`SELECT COUNT(1) FROM users WHERE id = ?`, userID).Scan(&exists); e == nil && exists == 0 {
			return 0, sql.ErrNoRows
		}
		return 0, ErrInsufficient
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO txns(user_id, delta, balance_after, reason, ref_type, ref_id, site_id, created_at)
		VALUES(?,?,?,?,?,?,?,?)`, userID, delta, next, reason, refType, refID, siteID, now); err != nil {
		return 0, err
	}
	return next, nil
}

// Balance 读余额。
func Balance(db *sql.DB, userID int64) (int64, error) {
	var b int64
	err := db.QueryRow(`SELECT balance FROM users WHERE id = ?`, userID).Scan(&b)
	return b, err
}
