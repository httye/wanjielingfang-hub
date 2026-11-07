package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Event 是 outbox 里的一条事件。业务只负责 Emit，下游由 consumer 决定。
type Event struct {
	ID       int64
	SiteID   int64
	Type     string
	Payload  map[string]any
	Attempts int
}

// EventHandler 处理一类事件。
type EventHandler interface {
	Handle(ctx context.Context, ev Event) error
}

// EventHandlerFunc 让函数实现 EventHandler。
type EventHandlerFunc func(ctx context.Context, ev Event) error

func (f EventHandlerFunc) Handle(ctx context.Context, ev Event) error { return f(ctx, ev) }

// Events 是 outbox 发布器 + 消费者调度器。
type Events struct {
	DB   *sql.DB
	Log  *log.Logger
	Poll time.Duration
	Max  int // 最大尝试次数

	mu       sync.RWMutex
	handlers map[string][]EventHandler
}

// NewEvents 构造事件总线。
func NewEvents(db *sql.DB, logger *log.Logger) *Events {
	return &Events{DB: db, Log: logger, Poll: 500 * time.Millisecond, Max: 3, handlers: map[string][]EventHandler{}}
}

// Register 注册消费者。同一类型可以挂多个。
func (e *Events) Register(typ string, h EventHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[typ] = append(e.handlers[typ], h)
}

// Emit 在给定事务内写事件（与业务变更同生共死）。
func (e *Events) Emit(tx *sql.Tx, siteID int64, typ string, payload any, now int64) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO events(site_id, type, payload_json, status, attempts, next_at, created_at)
		VALUES(?,?,?,0,0,?,?)`, siteID, typ, string(b), now, now)
	return err
}

// EmitDirect 无事务场景（后台任务用）。
func (e *Events) EmitDirect(siteID int64, typ string, payload any, now int64) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = e.DB.Exec(`INSERT INTO events(site_id, type, payload_json, status, attempts, next_at, created_at)
		VALUES(?,?,?,0,0,?,?)`, siteID, typ, string(b), now, now)
	return err
}

// Run 启动消费者循环，直到 ctx 取消。
func (e *Events) Run(ctx context.Context) {
	t := time.NewTicker(e.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.drain(ctx)
		}
	}
}

func (e *Events) drain(ctx context.Context) {
	rows, err := e.DB.QueryContext(ctx, `SELECT id, site_id, type, payload_json, attempts
		FROM events WHERE status = 0 AND next_at <= ? ORDER BY id LIMIT 32`, time.Now().Unix())
	if err != nil {
		e.Log.Printf("events: query: %v", err)
		return
	}
	defer rows.Close()

	type job struct {
		ev  Event
		raw string
	}
	var jobs []job
	for rows.Next() {
		var j job
		var raw string
		if err := rows.Scan(&j.ev.ID, &j.ev.SiteID, &j.ev.Type, &raw, &j.ev.Attempts); err != nil {
			continue
		}
		_ = json.Unmarshal([]byte(raw), &j.ev.Payload)
		j.raw = raw
		jobs = append(jobs, j)
	}
	_ = rows.Err()

	for _, j := range jobs {
		e.mu.RLock()
		hs := append([]EventHandler(nil), e.handlers[j.ev.Type]...)
		e.mu.RUnlock()

		if len(hs) == 0 {
			// 没有消费者的事件直接归档，避免永久占用队列。
			_, _ = e.DB.Exec(`UPDATE events SET status = 1 WHERE id = ?`, j.ev.ID)
			continue
		}

		var perr error
		for _, h := range hs {
			if err := h.Handle(ctx, j.ev); err != nil {
				perr = err
				break
			}
		}
		now := time.Now().Unix()
		if perr == nil {
			_, _ = e.DB.Exec(`UPDATE events SET status = 1 WHERE id = ?`, j.ev.ID)
			continue
		}
		attempts := j.ev.Attempts + 1
		if attempts >= e.Max {
			e.Log.Printf("events: %s id=%d dead after %d attempts: %v", j.ev.Type, j.ev.ID, attempts, perr)
			_, _ = e.DB.Exec(`UPDATE events SET status = 3, attempts = ? WHERE id = ?`, attempts, j.ev.ID)
			continue
		}
		backoff := int64(5 << attempts) // 10s, 20s, 40s
		if backoff > 300 {
			backoff = 300
		}
		_, _ = e.DB.Exec(`UPDATE events SET attempts = ?, next_at = ? WHERE id = ?`, attempts, now+backoff, j.ev.ID)
	}
}
