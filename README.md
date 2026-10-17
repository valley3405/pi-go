# goagent

参考 [pi](https://github.com/earendil-works/pi)（pi agent harness）架构，用 Go 实现的 AI coding agent。核心运行时仅使用标准库，终端 UI 基于 [Bubble Tea](https://github.com/charmbracelet/bubbletea)。

架构设计与 pi 的逐层映射见 [DESIGN.md](DESIGN.md)。

## 结构

```
pkg/llm       统一多供应商 LLM 层：消息模型、流式事件流、Provider 注册表
              （OpenAI 兼容 / Anthropic / 测试用 FauxProvider）
pkg/agent     Agent 运行时：双层循环、steering/follow-up 双队列、
              生命周期事件、工具三段管线（prepare/execute/finalize）
pkg/harness   append-only JSONL 树状会话、分支、上下文压缩、
              skills 发现与注入（SKILL.md + frontmatter，gitignore 感知）
pkg/tools     内置编码工具：bash / read / write / edit / ls / grep
pkg/tui       Bubble Tea 全屏客户端：流式对话、工具状态、多行输入、会话命令
cmd/goagent   CLI：TUI（默认）、行式 REPL（-repl）与 print 模式
examples/     无 API key 的端到端演示（FauxProvider）
```

## 构建与测试

```bash
go build ./...
go test ./...
go run ./examples/fauxdemo   # 无需 API key 的演示
```

## 使用

```bash
# print 模式（一次性）
export OPENAI_API_KEY=sk-...
go run ./cmd/goagent -p "列出当前目录并说明这是个什么项目"

# Anthropic
export ANTHROPIC_API_KEY=sk-ant-...
go run ./cmd/goagent -provider anthropic -model claude-sonnet-4-5 -p "..."

# OpenAI 兼容服务（DeepSeek / 本地网关等）
export OPENAI_BASE_URL=https://api.deepseek.com/v1
go run ./cmd/goagent -model deepseek-chat -p "..."

# TUI 交互模式（无参数启动）
go run ./cmd/goagent
# Enter 发送；Shift+Enter / Alt+Enter / Ctrl+J 换行
# PgUp/PgDn 或鼠标滚轮浏览历史；Esc/Ctrl+C 中止；空闲时 Ctrl+C 退出
# thinking 和工具轨迹默认活动中展开、结束后自动折叠
# Ctrl+O 手动展开/折叠轨迹；/trace auto 恢复自动模式
# 状态栏显示最近 input/output、当前分支累计 total/cache 命中率；/usage 查看明细
# 命令：/help  /skills  /branch <id>  /tree  /compact  /resume  /usage  /quit
# 轨迹：/trace auto|expand|collapse；!cmd 直接跑 bash

# 行式 REPL 回退模式
go run ./cmd/goagent -repl
```

## Skills

在 `~/.goagent/skills/<name>/SKILL.md`（全局）或当前项目 `./agents/skills/<name>/SKILL.md`（项目级）放一个带 frontmatter 的文件即可注册技能：

```markdown
---
name: commit
description: Write good commit messages following conventional commits
---

# Commit

（技能正文：流程、规范、示例……）
```

启动时自动发现（尊重 `.gitignore`/`.ignore`，同名时项目目录覆盖全局），清单注入系统提示词；模型按需用 `read` 工具加载 SKILL.md 全文。交互模式用 `/skills` 查看已加载技能。

## 作为库嵌入

```go
faux := llm.NewFauxProvider("faux", script, 16)        // 或 NewOpenAIProvider / NewAnthropicProvider
reg := llm.NewRegistry()
reg.SetProvider(faux)
model, _ := reg.GetModel("faux", "faux-1")

a := agent.New(agent.Options{
    SystemPrompt: "You are a coding agent.",
    Model:        model,
    Tools:        tools.CodingTools(),
    StreamFn:     reg.StreamSimple,
})
a.Subscribe(func(ev agent.Event) { /* 生命周期事件 */ })
a.BeforeToolCall = func(ctx context.Context, c agent.BeforeToolCallContext) (*agent.BeforeToolCallResult, error) {
    return nil, nil // 返回 {Block: true} 可拦截危险工具
}

session, _ := harness.CreateSession("~/.goagent/sessions", ".")
h := harness.New(a, session, reg.StreamSimple, llm.StreamOptions{})
h.AutoCompactWindow = 128000   // 超阈值自动压缩

h.Prompt(ctx, "修复 main.go 里的编译错误")
a.WaitForIdle()
```

## 与 pi 的对应关系

| pi | goagent |
|---|---|
| `Agent` / `agent-loop` | `agent.Agent` / `runLoop` |
| steering / follow-up 队列 | `Agent.Steer()` / `Agent.FollowUp()` |
| `transformContext` / `convertToLlm` | `Agent.TransformContext` / `Agent.ConvertToLLM` |
| before/afterToolCall 钩子 | `Agent.BeforeToolCall` / `AfterToolCall` |
| `SessionManager`（树状 JSONL） | `harness.Session` |
| `branch()` / compaction | `Session.Branch()` / `harness.Compact()` |
| skills 资源发现与注入 | `harness.LoadSkills()` / `Harness.ApplySkills()` |
| faux provider（测试） | `llm.FauxProvider` |

## 路线图

- RPC/server 模式（pi 的 protocol/server/client 层）
- 工具权限与容器化运行指引
