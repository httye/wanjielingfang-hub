package task

import (
	"context"
	"database/sql"

	"github.com/mcai/hub/internal/core"
)

// MarkDone 把任务标记为完成：写产物路径、发首单赠送（如果还没发过）、发出 task.done 事件。
func MarkDone(ctx context.Context, d *core.Deps, taskID, workerID int64, jarName, logName string,
	passAt1, fixRounds, aiTokens int64) error {
	now := d.Unix()
	return core.Tx(ctx, d.DB, func(tx *sql.Tx) error {
		var (
			userID, cost, siteID int64
			status               string
		)
		if err := tx.QueryRow(`SELECT user_id, cost, site_id, status FROM tasks WHERE id = ?`, taskID).
			Scan(&userID, &cost, &siteID, &status); err != nil {
			return core.NewErr(404, "not_found", "任务不存在")
		}
		if status == "done" {
			return nil // 幂等
		}
		if _, err := tx.Exec(`UPDATE tasks SET status = 'done', jar_path = ?, log_path = ?, finished_at = ?,
			pass_at_1 = ?, fix_rounds = ?, ai_tokens = ?, error = '', fail_stage = '', lease_until = 0
			WHERE id = ?`, jarName, logName, now, passAt1, fixRounds, aiTokens, taskID); err != nil {
			return err
		}

		// 注册赠送延后到首单成功：从没发过才发，且只发一次。
		var granted int64
		if err := tx.QueryRow(`SELECT signup_granted FROM users WHERE id = ?`, userID).Scan(&granted); err != nil {
			if err != sql.ErrNoRows {
				return err
			}
			return core.NewErr(404, "not_found", "任务所属用户不存在")
		}
		if granted == 0 {
			bonus := d.Cfg.Int("billing", "signup_bonus", 20)
			if bonus > 0 {
				if _, err := core.AddBalance(tx, userID, bonus, "signup_bonus", "task", taskID, siteID, now); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE users SET signup_granted = 1 WHERE id = ?`, userID); err != nil {
				return err
			}
		}

		return d.Ev.Emit(tx, siteID, "task.done", map[string]any{
			"task_id": taskID, "user_id": userID, "worker_id": workerID,
			"pass_at_1": passAt1, "fix_rounds": fixRounds, "cost": cost,
		}, now)
	})
}

// MarkFailed 处理失败：按策略退款（成本 - 失败成本），标记可免费重试一次，发 task.failed 事件。
func MarkFailed(ctx context.Context, d *core.Deps, taskID, workerID int64, stage, msg string) error {
	now := d.Unix()
	return core.Tx(ctx, d.DB, func(tx *sql.Tx) error {
		var (
			userID, cost, siteID, refunded int64
			status                         string
		)
		if err := tx.QueryRow(`SELECT user_id, cost, site_id, status, refunded FROM tasks WHERE id = ?`, taskID).
			Scan(&userID, &cost, &siteID, &status, &refunded); err != nil {
			return core.NewErr(404, "not_found", "任务不存在")
		}
		if status == "failed" || status == "done" {
			return nil // 幂等
		}
		if refunded == 1 {
			return nil // 已经退过，不重复退
		}

		credit := d.Cfg.Int("billing", "credit_cost_fail", 1)
		refund := cost - credit
		if refund < 0 {
			refund = 0
		}
		if refund > 0 {
			if _, err := core.AddBalance(tx, userID, refund, "task_refund", "task", taskID, siteID, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE tasks SET status = 'failed', error = ?, fail_stage = ?, refunded = 1,
			finished_at = ?, lease_until = 0 WHERE id = ?`, msg, stage, now, taskID); err != nil {
			return err
		}
		return d.Ev.Emit(tx, siteID, "task.failed", map[string]any{
			"task_id": taskID, "user_id": userID, "worker_id": workerID,
			"stage": stage, "error": msg, "refund": refund, "credit": credit,
		}, now)
	})
}

// ReclaimExpired 回收租约过期的 running 任务：
// 次数未用尽 → 回队列；已用尽 → 判失败走退款，避免任务永久卡在 pending。
func ReclaimExpired(ctx context.Context, d *core.Deps) {
	now := d.Unix()
	rows, err := d.DB.QueryContext(ctx, `SELECT id, attempts FROM tasks
		WHERE status = 'running' AND lease_until > 0 AND lease_until < ?`, now)
	if err != nil {
		return
	}
	type expired struct{ id, attempts int64 }
	var list []expired
	for rows.Next() {
		var it expired
		if rows.Scan(&it.id, &it.attempts) == nil {
			list = append(list, it)
		}
	}
	rows.Close()

	for _, it := range list {
		if it.attempts >= 3 {
			if err := MarkFailed(ctx, d, it.id, 0, "lease", "租约超时且重试次数已用尽"); err != nil {
				d.Log.Printf("reclaim fail task=%d: %v", it.id, err)
			}
			continue
		}
		if _, err := d.DB.Exec(`UPDATE tasks SET status = 'pending', worker_id = 0, lease_until = 0
			WHERE id = ? AND status = 'running'`, it.id); err != nil {
			d.Log.Printf("reclaim task=%d: %v", it.id, err)
		}
	}
}
