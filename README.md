# goagent

参考 [pi](https://github.com/earendil-works/pi)（pi agent harness）架构，用 Go 实现的 AI coding agent。零外部依赖（仅标准库）。

架构设计与 pi 的逐层映射见 [DESIGN.md](DESIGN.md)。

## 结构

```
pkg/llm       统一多供应商 LLM 层：消息模型、流式事件流、Provider 注册表
              （OpenAI 兼容 / Anthropic / 测试用 FauxProvider）
pkg/agent     Agent 运行时：双层循环、steering/follow-up 双队列、
              生命周期事件、工具三段管线（prepare/execute/finalize）
pkg/harness   append-only JSONL 树状会话、分支、上下文压缩
pkg/tools     内置编码工具：bash / read / write / edit / ls / grep
cmd/goagent   CLI：interactive REPL 与 print 模式
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

# 交互模式（无参数启动）
go run ./cmd/goagent
# 命令：/branch <id>  /tree  /compact  /resume  /quit ；!cmd 直接跑 bash
```

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
| faux provider（测试） | `llm.FauxProvider` |

## 路线图

- 行式 REPL → 完整 TUI（差分渲染）
- RPC/server 模式（pi 的 protocol/server/client 层）
- 工具权限与容器化运行指引
