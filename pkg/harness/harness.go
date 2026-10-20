package harness

import (
	"context"
	"fmt"
	"sync"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// SkillDir 是一个技能扫描根及其来源标注（如 "global"/"project"）。
type SkillDir struct {
	Root   string
	Source string
}

// Harness 把 Agent 与树状 Session 组装在一起（对应 pi 的 AgentSession 的精简版）：
// 订阅 agent 事件自动落盘，提供 Prompt/Branch/Resume/Compact。
type Harness struct {
	Agent   *agent.Agent
	Session *Session

	streamFn   llm.StreamFn
	streamOpts llm.StreamOptions
	// AutoCompactWindow 大于 0 时，prompt 前按该窗口自动判断是否压缩。
	AutoCompactWindow int
	KeepRecent        int // compaction 时保留的最近消息数

	skillsMu         sync.RWMutex
	skills           []Skill
	skillWarnings    []string
	skillDirs        []SkillDir
	systemPromptBase string
	promptBaseSet    bool
	skillToolAdded   bool
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

// ApplySkills 发现技能并注入 agent（对应 pi 的 skills 资源注入）：
// 清单追加进系统提示词，同时注册只读的 skill 工具供模型按名加载全文。
// 后传入的 SkillDir 覆盖先传入的同名技能。重复调用是幂等的
// （系统提示词基于首次调用时的原文重建，不会重复追加）。
func (h *Harness) ApplySkills(dirs ...SkillDir) {
	h.skillsMu.Lock()
	h.skillDirs = append([]SkillDir(nil), dirs...)
	h.skillsMu.Unlock()
	h.reloadSkills()
}

// ReloadSkills 按上次 ApplySkills 的目录重新扫描技能（热重载）。
func (h *Harness) ReloadSkills() {
	h.reloadSkills()
}

func (h *Harness) reloadSkills() {
	h.skillsMu.RLock()
	dirs := append([]SkillDir(nil), h.skillDirs...)
	h.skillsMu.RUnlock()

	var lists [][]Skill
	var warnings []string
	for _, d := range dirs {
		skills, warns, _ := LoadSkills(d.Root, d.Source)
		lists = append(lists, skills)
		warnings = append(warnings, warns...)
	}
	merged := MergeSkills(lists...)

	h.skillsMu.Lock()
	h.skills = merged
	h.skillWarnings = warnings
	base := h.systemPromptBase
	baseSet := h.promptBaseSet
	toolAdded := h.skillToolAdded
	h.skillsMu.Unlock()

	snap := h.Agent.Snapshot()
	if !baseSet {
		base = snap.SystemPrompt
	}
	h.Agent.SetSystemPrompt(base + FormatForSystemPrompt(merged))
	if !toolAdded {
		h.Agent.SetTools(append(h.Agent.Tools(), skillTool{h: h}))
	}

	h.skillsMu.Lock()
	h.systemPromptBase = base
	h.promptBaseSet = true
	h.skillToolAdded = true
	h.skillsMu.Unlock()
}

// SkillsList 返回当前技能清单副本。
func (h *Harness) SkillsList() []Skill {
	h.skillsMu.RLock()
	defer h.skillsMu.RUnlock()
	return append([]Skill(nil), h.skills...)
}

// SkillWarnings 返回最近一次扫描中解析失败的技能警告。
func (h *Harness) SkillWarnings() []string {
	h.skillsMu.RLock()
	defer h.skillsMu.RUnlock()
	return append([]string(nil), h.skillWarnings...)
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
