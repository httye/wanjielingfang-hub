package site

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mcai/hub/internal/core"
	"github.com/mcai/hub/internal/modules/task"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Messages  []chatMessage `json:"messages"`
	Mode      string        `json:"mode"` // restate | generate | iterate
	SessionID int64         `json:"session_id"`
}

// ---------------------------------------------------------------------------
// SSE 对话
// ---------------------------------------------------------------------------

func (m *Module) aiChat(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req chatReq
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if len(req.Messages) == 0 {
			core.Fail(w, core.Errf("bad_messages", "缺少对话内容"))
			return
		}
		u, err := d.Auth.MustResolve(r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		s := core.SiteFrom(r)

		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache, no-transform")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)

		send := func(v any) bool {
			b, _ := json.Marshal(v)
			if _, err := io.WriteString(w, "data: "+string(b)+"\n\n"); err != nil {
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
			return true
		}

		// 记录用户提问（会话隔离在查询侧按 user_id 过滤）
		sid := m.recordMessage(d, req.SessionID, u.ID, s.ID, "user", lastContent(req.Messages))

		var answer strings.Builder
		if d.AI.Mock || d.AI.APIKey == "" {
			mock := mockReply(req)
			for _, part := range chunkRunes(mock, 24) {
				if !send(map[string]any{"delta": part}) {
					return
				}
			}
			answer.WriteString(mock)
		} else {
			ok, text, tokens := m.streamUpstream(r.Context(), d, req.Messages, func(delta string) bool {
				answer.WriteString(delta)
				return send(map[string]any{"delta": delta})
			})
			if !ok {
				// 上游失败：只发 error，绝不再发 done，避免前端把失败当成功。
				send(map[string]any{"error": "upstream_failed", "message": "上游模型调用失败"})
				return
			}
			m.addUsage(d, s.ID, tokens)
			_ = text
		}

		m.recordMessage(d, sid, u.ID, s.ID, "assistant", answer.String())
		send(map[string]any{"done": true, "session_id": sid})
	}
}

func lastContent(msgs []chatMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Content
}

func chunkRunes(s string, n int) []string {
	r := []rune(s)
	if n <= 0 {
		n = 24
	}
	var out []string
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

// streamUpstream 调 OpenAI 兼容接口并按 SSE 转发增量。
func (m *Module) streamUpstream(ctx context.Context, d *core.Deps, msgs []chatMessage, onDelta func(string) bool) (bool, string, int64) {
	payload := map[string]any{
		"model":          d.AI.Model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true}, // 没有它很多上游不回 usage，ai_tokens 恒为 0
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, "", 0
	}
	url := strings.TrimRight(d.AI.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, "", 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.AI.APIKey)

	resp, err := d.HTTP.Do(req)
	if err != nil {
		d.Log.Printf("ai upstream: %v", err)
		return false, "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		d.Log.Printf("ai upstream %d: %s", resp.StatusCode, string(raw))
		return false, "", 0
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var full strings.Builder
	var tokens int64
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			tokens = chunk.Usage.TotalTokens
		}
		if len(chunk.Choices) > 0 {
			piece := chunk.Choices[0].Delta.Content
			if piece == "" {
				continue
			}
			full.WriteString(piece)
			if !onDelta(piece) {
				return true, full.String(), tokens
			}
		}
	}
	return true, full.String(), tokens
}

func (m *Module) addUsage(d *core.Deps, siteID, tokens int64) {
	if siteID == 0 || tokens <= 0 {
		return
	}
	// 粗略成本：1 token ≈ 0.002 积分，便于后台观察趋势。
	cost := tokens * 2 / 1000
	_, _ = d.DB.Exec(`UPDATE sites SET ai_tokens = ai_tokens + ?, ai_cost = ai_cost + ? WHERE id = ?`,
		tokens, cost, siteID)
}

// recordMessage 落库一条对话，返回会话 ID（新建的会话要回传给调用方复用，
// 否则同一轮问答会插出两条 chat_sessions）。
func (m *Module) recordMessage(d *core.Deps, sessionID, userID, siteID int64, role, content string) int64 {
	if content == "" {
		return sessionID
	}
	now := d.Unix()
	if sessionID <= 0 {
		res, err := d.DB.Exec(`INSERT INTO chat_sessions(user_id, site_id, title, created_at, updated_at)
			VALUES(?,?,?,?,?)`, userID, siteID, truncate(content, 40), now, now)
		if err != nil {
			return sessionID
		}
		sessionID, _ = res.LastInsertId()
	}
	_, _ = d.DB.Exec(`INSERT INTO chat_messages(session_id, role, content, tokens, created_at) VALUES(?,?,?,0,?)`,
		sessionID, role, content, now)
	_, _ = d.DB.Exec(`UPDATE chat_sessions SET updated_at = ? WHERE id = ?`, now, sessionID)
	return sessionID
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---------------------------------------------------------------------------
// 生成并入队
// ---------------------------------------------------------------------------

func (m *Module) aiGenerate(d *core.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Platform    string        `json:"platform"`
			MCVersion   string        `json:"mc_version"`
			Requirement string        `json:"requirement"`
			Messages    []chatMessage `json:"messages"`
			SessionID   int64         `json:"session_id"`
		}
		if err := core.ReadJSON(r, &req); err != nil {
			core.Fail(w, err)
			return
		}
		if strings.TrimSpace(req.Requirement) == "" && len(req.Messages) == 0 {
			core.Fail(w, core.Errf("bad_requirement", "缺少需求描述"))
			return
		}
		u, err := d.Auth.MustResolve(r)
		if err != nil {
			core.Fail(w, err)
			return
		}
		s := core.SiteFrom(r)

		src := ""
		tokens := int64(0)
		if d.AI.Mock || d.AI.APIKey == "" {
			src = mockPlugin(req.Platform, req.MCVersion, req.Requirement)
		} else {
			msgs := req.Messages
			if len(msgs) == 0 {
				msgs = []chatMessage{{Role: "user", Content: req.Requirement}}
			}
			msgs = append([]chatMessage{{
				Role: "system",
				Content: fmt.Sprintf("你是 Minecraft 插件工程师。为 %s / MC %s 生成完整可编译的 Java 源码，"+
					"只输出代码，不要解释。必须包含 plugin.yml 所需的 main 类与 onEnable/onCommand 实现。",
					req.Platform, req.MCVersion),
			}}, msgs...)
			ok, text, tk := m.streamUpstream(r.Context(), d, msgs, func(string) bool { return true })
			if !ok || text == "" {
				core.Fail(w, core.NewErr(http.StatusBadGateway, "ai_failed", "生成失败，请稍后重试"))
				return
			}
			src = stripCodeFence(text)
			tokens = tk
			m.addUsage(d, s.ID, tokens)
		}

		idem := r.Header.Get("Idempotency-Key")
		id, cost, err := task.Create(r.Context(), d, task.CreateReq{
			UserID:    u.ID,
			SiteID:    s.ID,
			Kind:      "generate",
			Platform:  req.Platform,
			MCVersion: req.MCVersion,
			Src:       src,
			Payload:   map[string]any{"requirement": req.Requirement, "session_id": req.SessionID},
			IdemKey:   idem,
		})
		if err != nil {
			core.Fail(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, map[string]any{
			"task_id": id, "cost": cost, "status": "pending", "ai_tokens": tokens,
			"download_hint": "/api/v1/tasks/" + fmt.Sprint(id) + "/download",
		})
	}
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// Mock（无密钥时也能跑通全链路）
// ---------------------------------------------------------------------------

func mockReply(req chatReq) string {
	if req.Mode == "restate" || len(req.Messages) <= 1 {
		last := lastContent(req.Messages)
		return "我理解你的需求是：" + truncate(last, 200) +
			"\n\n将实现以下能力：\n1. 注册指令与权限节点\n2. 读写 config.yml 保存设置\n3. 事件监听与消息反馈\n\n" +
			"确认无误后点击「确认需求并生成」，我会输出完整源码。"
	}
	return "已根据你的描述调整实现，主要变更：命令参数解析、配置热重载、权限校验。"
}

func mockPlugin(platform, version, requirement string) string {
	name := "GeneratedPlugin"
	return `package com.mcai.generated;

import org.bukkit.command.Command;
import org.bukkit.command.CommandSender;
import org.bukkit.plugin.java.JavaPlugin;

/**
 * Generated for ` + platform + ` / MC ` + version + `
 * Requirement: ` + truncate(requirement, 120) + `
 */
public final class ` + name + ` extends JavaPlugin {

    @Override
    public void onEnable() {
        saveDefaultConfig();
        getLogger().info("[MC-AI] plugin enabled on " + getServer().getVersion() + "");
    }

    @Override
    public void onDisable() {
        getLogger().info("[MC-AI] plugin disabled");
    }

    @Override
    public boolean onCommand(CommandSender sender, Command command, String label, String[] args) {
        if (!sender.hasPermission("mcai.use")) {
            sender.sendMessage("\u00a7c权限不足");
            return true;
        }
        sender.sendMessage("\u00a7a" + getConfig().getString("message", "hello from MC-AI"));
        return true;
    }
}
`
}

// contextT 已移除（不再需要别名）。
