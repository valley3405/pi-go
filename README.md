# goagent

参考 [pi](https://github.com/earendil-works/pi)（pi agent harness）架构，用 Go 实现的 AI coding agent。核心运行时仅使用标准库，终端 UI 基于 [Bubble Tea](https://github.com/charmbracelet/bubbletea)。

架构设计与 pi 的逐层映射见 [DESIGN.md](DESIGN.md)。

## 结构

```
pkg/llm       统一多供应商 LLM 层：消息模型、流式事件流、Provider 注册表
              （OpenAI 兼容 / Anthropic / 测试用 FauxProvider）
pkg/agent     Agent 运行时：双层循环、steering/follow-up 双队列、
              生命周期事件、运行预算、工具管线与统一输出治理
pkg/permission 独立权限层：权限模式、工具/Bash 命令 allow/deny 规则、交互审批与 deny 优先
pkg/harness   append-only JSONL 树状会话、分支、上下文压缩、
              skills 发现与注入（SKILL.md + frontmatter，gitignore 感知）
pkg/tools     内置编码工具：bash / read / write / edit / multi_edit / ls / grep / glob / web_search / web_fetch
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

## Permissions

默认 CLI 启用工作区约束和权限控制：工作区内 `read` / `ls` / `grep` / `glob` 自动允许，`write` / `edit` / `multi_edit` / `bash` 默认需要审批。`web_search` / `web_fetch` 访问网络且按次计费，同样默认需要审批，可用 `-allow web_search` 等规则显式放权。工作区外读取必须单次审批，文件写入直接拒绝。审批面板始终展示完整参数（TUI 中可用 PgUp/PgDn 滚动、Ctrl+O 折叠/展开），不存在"仅展示前缀"的审批；选择需显式进行：TUI 中用 ↑/↓ 或 1/2 选择"允许/拒绝"后按 Enter 确认（默认选中"拒绝"），REPL 中输入 1 允许、2 拒绝。TUI 还可用 `Shift+Tab` 在 `default → acceptEdits → auto → plan` 间循环，状态栏以 `perm:<mode>` 显示当前模式；`bypass` 和 `dontAsk` 只能由启动参数设置，从这些模式按 `Shift+Tab` 会回到 `default`。可用模式：

- `default`：只读工具自动允许，其他工具按规则或审批结果执行。
- `acceptEdits`：额外自动允许 `write` / `edit` / `multi_edit`。
- `auto`：在 acceptEdits 基础上自动执行常规只读/开发命令；工作区内的删除、移动和修改（包括工作区位于 `/tmp` 时）也自动执行。越出工作区、网络、提权、系统/进程控制、包安装、Git 写入和未知命令仍需询问。
- `plan`：仅允许只读工具。
- `dontAsk`：不发起审批，只允许只读工具和 allow 规则命中的工具。
- `bypass`：除 deny 规则外全部允许；`-yes` 是此模式的简写。

```bash
go run ./cmd/goagent -permission acceptEdits
go run ./cmd/goagent -permission auto
go run ./cmd/goagent -allow bash
go run ./cmd/goagent -yes -deny bash   # deny 始终优先
go run ./cmd/goagent -yes -p "修改文件并运行测试"
```

`-allow` / `-deny` 支持重复传入或逗号分隔，规则为精确工具名或 `*`。print 模式不会隐式等待 stdin；需要写文件或执行 Bash 时，必须通过 `-yes`、`-allow` 或相应权限模式显式放权。

### Bash 命令规则

可用 `-allow-command` / `-deny-command` 按正则控制 Bash 命令，规则可重复传入且不会按逗号拆分：

```bash
go run ./cmd/goagent -allow-command '^go test( .*)?$'
go run ./cmd/goagent -yes -deny-command 'rm\s+-rf'
```

- allow 命令规则自动锚定完整命令；命中后免审批。
- 命令包含 shell 控制符、管道、重定向或命令替换时，即使 allow 正则命中也仍需审批，避免 `go test && rm -rf ...` 被普通规则误放行。
- deny 命令规则按部分匹配，优先级高于 `-yes`、`bypass`、工具 allow 规则和审批。
- `plan` 仍禁止全部 Bash；`dontAsk` 和 print 下仅 allow 命中的命令可自动执行。

命令规则不是沙箱，宽正则仍可能带来风险，应尽量缩小范围。

作为库嵌入时，普通工具的 `agent.Options.Permission` 为 nil 表示不启用权限评估，保持原有行为；传入 `&permission.Policy` 或自定义 `permission.Evaluator` 即可启用。工作区工具即使未配置 Evaluator，也会执行默认权限检查。权限系统不等于容器沙箱。

### 工作区目录

工作区默认为启动目录，可通过 `-workspace /path/to/project` 指定。相对文件路径和 shell 工作目录都以该目录为基准；工作区内的正常符号链接可用，指向外部的链接按真实目标判定权限。

| 文件访问 | 行为 |
|---|---|
| 工作区内读取、列目录、搜索 | 按现有只读规则自动允许 |
| 工作区内写入 | 按当前模式、规则或审批执行 |
| 工作区外读取、列目录、搜索 | 必须单次审批，`plan`、`bypass`、`-yes`、`-allow read` 不能自动放行 |
| 工作区外文件写入 | 所有模式均拒绝 |
| `dontAsk` 或 print 模式的工作区外读取 | 拒绝，不等待审批 |

目录审批只覆盖被批准目录的真实子树；递归搜索不会读取指向授权范围外的链接。审批绑定本次解析出的目标，之后替换符号链接不能扩大授权范围。TUI 在可滚动轨迹中展示完整审批路径，REPL 展示完整路径和参数。

库嵌入可显式创建受限工具集：

```go
workspace, err := tools.NewWorkspace(projectDir)
if err != nil { /* 处理错误 */ }
defer workspace.Close() // 所有 Agent 调用结束后关闭
toolList := workspace.CodingTools() // 或 workspace.ReadOnlyTools()
// 将 toolList 传给 agent.Options.Tools，并配置 Permission/Approver。
```

底层 `tools.CodingTools()` 和单独的 `tools.Read()` 等构造函数保留原有路径行为。安全目录访问目前支持 Unix，其他平台会明确返回不支持错误；复用已有 `golang.org/x/sys/unix` 提供 Go 1.23 所需的目录句柄操作。特殊文件和硬链接写入会被拒绝。`bash` 仅固定工作目录，仍能访问宿主权限允许的其他路径；需要对 shell 强制隔离时应使用容器或操作系统沙箱。

## 运行预算与终止原因

```bash
go run ./cmd/goagent -max-turns 50 -max-duration 10m
go run ./cmd/goagent -max-turns 0 -max-duration 0   # 均不限制
```

CLI 默认最多 50 次 LLM 响应、不限时长；库嵌入时 `MaxTurns` / `MaxDuration` 零值均为不限制。每次 assistant 响应计一个 turn，工具后续回合和截断恢复也计入预算。

`agent_end` 与 `Agent.Snapshot().TerminationReason` 会给出 `completed`、`aborted`、`timeout`、`max_turns`、`output_limit`、`model_error`、`prompt_too_long` 或 `error`。无工具调用的输出截断最多自动续写恢复 3 次，耗尽后以 `output_limit` 结束。

TUI 和 REPL 会显示非 completed 原因；print 退出码为：正常 0、超时 124、用户中止 130，其余错误/预算耗尽为 1。

## 工具输出与编码工具

Agent 在工具 finalize 阶段统一限制输出，`MaxToolOutputChars` 默认 30000 字符，内置和自定义工具都适用。未配置 `ToolOutputDir` 时按 rune 安全地进行头尾截断；配置后将完整文本写入 0600 文件，模型只接收头部、省略说明和文件路径。

```bash
# CLI 默认把超限完整输出保存到 ~/.goagent/tool-output
go run ./cmd/goagent -max-tool-output 30000

# 关闭落盘（仍为头尾截断）
go run ./cmd/goagent -tool-output-dir ""
```

内置工具新增：`glob` 支持 `*`、`?`、`**` 文件查找；`multi_edit` 在同一文件内顺序应用多组替换，任一失败都不会写入部分结果。库嵌入时 `ToolOutputDir` 默认为空。

## Skills

在 `~/.goagent/skills/<name>/SKILL.md`（全局）或当前项目 `./.agents/skills/<name>/SKILL.md`（项目级；`./agents/skills/` 作为兼容别名也会扫描）放一个带 frontmatter 的文件即可注册技能：

```markdown
---
name: commit
description: Write good commit messages following conventional commits
---

# Commit

（技能正文：流程、规范、示例……）
```

启动时自动发现（尊重 `.gitignore`/`.ignore`，同名时项目目录覆盖全局），清单注入系统提示词；模型按需用内置的只读 `skill` 工具按名加载 SKILL.md 全文——不经过文件读取权限，因此全局技能（位于工作区外）加载时不会触发审批。解析失败的技能会在启动时输出 warning，并可在 `/skills` 中查看；`/skills reload` 不重启即可重新扫描技能目录。上下文压缩时，最近一次加载的技能正文会按预算保留在压缩摘要中，不会因压缩丢失。

注意：项目级技能随仓库分发，其内容会作为指令进入模型上下文。系统提示词已将项目技能标注为不可信内容，但评审陌生仓库的 `.agents/skills/` 仍是好习惯。

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

## Provider 网络配置

OpenAI 兼容和 Anthropic Provider 默认对成功响应头到达前的临时网络错误、429 和 5xx 最多重试 2 次，采用带抖动的指数退避（250ms 起步、4s 上限），等待可被 context 取消。支持 `Retry-After` 秒数和 HTTP 日期；服务端要求的等待超过上限时直接返回错误，不提前重试。收到成功响应头后，即使 SSE 中途断开或超时也不会重放请求，未完成的 EOF 会作为错误返回。

```go
transport := http.DefaultTransport.(*http.Transport).Clone()
transport.ResponseHeaderTimeout = 30 * time.Second
client := &http.Client{Transport: transport} // 保留默认的环境变量代理配置

p := llm.NewOpenAIProvider("openai", baseURL, models,
    llm.WithHTTPClient(client),
    llm.WithRetryPolicy(llm.RetryPolicy{
        MaxRetries: 2, InitialDelay: 250 * time.Millisecond, MaxDelay: 4 * time.Second,
    }),
)
// NewAnthropicProvider(id, models, options...) 接收相同选项。
// WithRetryPolicy(llm.RetryPolicy{MaxRetries: 0}) 可关闭重试。
```

`http.Client.Timeout` 会覆盖整个 SSE 读取过程；只限制建连或响应头等待时，应配置 Transport。默认 client 不设总超时，整体时限由调用 context 或 Agent 的 `MaxDuration` 控制。

如需统一限制多个 Provider 或并发调用的请求速率，可向它们传入同一个 `WithRequestLimiter(limiter)`。限流器实现 `Wait(context.Context) error`，必须支持并发和取消；每次 HTTP 尝试（包括重试）都会先等待许可。默认不限制速率。

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
- 容器化运行指引（权限系统不等于 sandbox）
