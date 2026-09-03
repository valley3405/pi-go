package harness

import (
	"context"
	"fmt"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// Harness 把 Agent 与树状 Session 组装在一起（对应 pi 的 AgentSession 的精简版）：
// 订阅 agent 事件自动落盘，提供 Prompt/Branch/Resume/Compact。
type Harness struct {
	Agent   *agent.Agent
	Session *Session

	streamFn   llm.StreamFn
	streamOpts llm.StreamOptions
	// Skills 是 ApplySkills 加载到的技能清单。
	Skills []Skill
	// AutoCompactWindow 大于 0 时，prompt 前按该窗口自动判断是否压缩。
	AutoCompactWindow int
	KeepRecent        int // compaction 时保留的最近消息数
}

// New 创建 Harness。session 可为 nil（纯内存模式，不落盘）。
func New(a *agent.Agent, s *Session, streamFn llm.StreamFn, opts llm.StreamOptions) *Harness {
	h := &Harness{
		Agent:      a,
		Session:    s,
		streamFn:   streamFn,
		streamOpts: opts,
		KeepRecent: 4,
	}
	if s != nil {
		// 事件落盘：每条完整消息 append 一个 entry（对应 pi 的 session 记录）
		a.Subscribe(func(ev agent.Event) {
			if ev.Type == agent.EventMessageEnd && ev.Message != nil {
				_, _ = s.AppendMessage(*ev.Message)
			}
		})
	}
	return h
}

// ApplySkills 把技能清单注入 agent 的系统提示词（对应 pi 的 skills 资源注入）。
// 模型看到清单后，用 read 工具读取对应 SKILL.md 按需加载全文。
// project 来源的技能覆盖 global 同名技能。
func (h *Harness) ApplySkills(globalDir, projectDir string) {
	global, _ := LoadSkills(globalDir, "global")
	project, _ := LoadSkills(projectDir, "project")
	skills := MergeSkills(global, project)
	h.Skills = skills
	if len(skills) == 0 {
		return
	}
	snap := h.Agent.Snapshot()
	h.Agent.SetSystemPrompt(snap.SystemPrompt + FormatForSystemPrompt(skills))
}

// Prompt 提交新 prompt。开启 AutoCompactWindow 时先做自动压缩。
func (h *Harness) Prompt(ctx context.Context, text string) error {
	if h.Session != nil && h.AutoCompactWindow > 0 {
		msgs, err := h.Session.Messages()
		if err == nil && ShouldCompact(msgs, h.AutoCompactWindow) {
			if _, err := h.Compact(ctx); err != nil {
				return fmt.Errorf("auto-compaction: %w", err)
			}
			if err := h.syncAgentMessages(); err != nil {
				return err
			}
		}
	}
	return h.Agent.Prompt(ctx, text)
}

// Compact 手动压缩当前分支。
func (h *Harness) Compact(ctx context.Context) (string, error) {
	if h.Session == nil {
		return "", fmt.Errorf("no session attached")
	}
	model := h.Agent.Snapshot().Model
	return Compact(ctx, h.Session, h.streamFn, model, h.streamOpts, h.KeepRecent)
}

// Branch 把会话分叉到历史 entry。
func (h *Harness) Branch(entryID string) error {
	if h.Session == nil {
		return fmt.Errorf("no session attached")
	}
	if err := h.Session.Branch(entryID); err != nil {
		return err
	}
	return h.syncAgentMessages()
}

// Resume 从会话文件恢复 agent 的 transcript（崩溃恢复 / 重启续聊）。
func (h *Harness) Resume() error {
	if h.Session == nil {
		return fmt.Errorf("no session attached")
	}
	return h.syncAgentMessages()
}

func (h *Harness) syncAgentMessages() error {
	msgs, err := h.Session.Messages()
	if err != nil {
		return err
	}
	h.Agent.SetMessages(msgs)
	return nil
}
