# goagent 架构设计

参考 [pi](https://github.com/earendil-works/pi)（pi agent harness）的分层架构，用 Go 从零实现一个可扩展的 AI coding agent。本文档先做架构映射与设计决策，再给出开发计划。

## 1. pi 架构回顾与 Go 映射

pi 的核心思想：**分层解耦 + 事件驱动 + 最小内核**。goagent 逐层对应：

| pi (TypeScript) | goagent (Go) | 职责 |
|---|---|---|
| `pi-ai` | `pkg/llm` | 统一多供应商 LLM API：消息模型、流式事件、工具调用、用量统计 |
| `pi-agent-core` → `Agent` / `agent-loop` | `pkg/agent` | 有状态 agent：双层循环、steering/follow-up 双队列、生命周期事件、工具钩子 |
| `agent/harness` + `session-manager` | `pkg/harness` | 会话持久化（append-only JSONL、树状分支）、上下文压缩（compaction） |
| `coding-agent/core/tools` | `pkg/tools` | 内置工具：bash / read / write / edit / ls / grep |
| 扩展系统（TS 模块） | 钩子接口 + 工具注册（Go interface） | Go 没有动态加载 TS 的能力，改用编译期接口注入 |
| `pi-tui` | `pkg/tui` + `cmd/goagent` | Bubble Tea 全屏 TUI；保留 stdlib 行式 REPL 作为回退 |
| RPC / server / client | 路线图 | 暂不进内核 |

关键约束：核心 LLM、agent、harness 使用 Go 标准库（`net/http` + SSE 手工解析、`encoding/json`、`os/exec`）；工具层的安全目录访问复用已有 `golang.org/x/sys/unix`，以在 Go 1.23 上支持基于目录句柄的访问；TUI 使用 Bubble Tea/Bubbles/Lip Gloss，并固定兼容 Go 1.23 的 v1 版本。

## 2. 分层结构

```
┌──────────────────────────────────────────────────┐
│ cmd/goagent        CLI（TUI / REPL / print）        │
├──────────────────────────────────────────────────┤
│ pkg/tui            Bubble Tea 模型、事件桥接与全屏布局 │
├──────────────────────────────────────────────────┤
│ pkg/harness        会话树（JSONL）、分支、压缩、skills │
├──────────────────────────────────────────────────┤
│ pkg/agent          Agent 状态机 + agent loop       │
│  ├─ Agent            状态、事件订阅、双队列         │
│  └─ loop             runLoop：外层 follow-up /     │
│                      内层 tool-call 循环           │
├──────────────────────────────────────────────────┤
│ pkg/permission     权限模式、规则与宿主审批           │
├──────────────────────────────────────────────────┤
│ pkg/llm            Provider 抽象 + 流式事件流       │
│  ├─ Provider 接口    OpenAI 兼容 / Anthropic       │
│  ├─ EventStream      channel 化的事件流            │
│  └─ FauxProvider     脚本化假模型（测试用）         │
├──────────────────────────────────────────────────┤
│ pkg/tools          bash/read/write/edit/ls/grep    │
└──────────────────────────────────────────────────┘
```

依赖方向严格自上而下：`agent` 不认识具体 provider（只认 `llm.StreamFn`），`harness` 不认识 CLI/TUI，`tui` 只通过 harness/agent 公开接口工作，`llm` 不认识 agent。

## 3. pkg/llm —— 统一 LLM 层（≈ pi-ai）

### 3.1 消息模型

pi 区分 `AgentMessage`（宽）与 LLM `Message`（窄）。Go 版同样处理：

- `llm.Message`：`role(user/assistant/toolResult)` + `[]Content`。
- `Content` 用接口 + 具体类型：`TextContent`、`ThinkingContent`、`ToolCallContent`（含 id/name/arguments JSON）、`ImageContent`（base64）。
- `AssistantMessage` 附带 `Usage`（input/output/cacheRead/cacheWrite/totalTokens + cost）、`StopReason`（`stop/length/toolUse/error/aborted`）。Input 统一为含缓存读写的总输入；`Total()` 在 provider 未返回 total 时回退到 input+output，`Add()` 用于跨响应累计，`CacheHitRate()` 按 cacheRead/input 计算命中率。
- `llm.Tool`：name、description、JSON Schema（`map[string]any` 表示，零依赖）。

### 3.2 流式事件

对应 pi 的 `AssistantMessageEvent`：

```
start → (text_start/text_delta/text_end)*
      → (thinking_start/thinking_delta/thinking_end)*
      → (toolcall_start/toolcall_delta/toolcall_end)*
      → done | error
```

`EventStream` 用 `chan Event` + 缓冲 + `Result()` 实现，事件里携带**累积中的 partial message 快照**（与 pi 一致，UI 直接渲染 partial 即可）。

### 3.3 Provider 抽象

```go
type StreamFn func(ctx context.Context, model Model, llmCtx Context, opts StreamOptions) *EventStream

type Provider interface {
    ID() string
    Models() []Model
    Stream(ctx context.Context, model Model, c Context, opts StreamOptions) *EventStream
}
```

- `Registry`（≈ pi 的 `createModels()`）：注册 provider、按 `provider/model-id` 查模型。
- 内置 `OpenAIProvider`（兼容 OpenAI / DeepSeek / 各类网关，SSE 流式 + tool_calls 增量合并）与 `AnthropicProvider`（SSE，`content_block_*` 事件映射）。两者解析流式 usage；OpenAI 支持标准/DeepSeek 缓存字段，Anthropic 支持嵌套 message_start usage，并在完成时把 cacheRead/cacheWrite 合入 Input 以统一口径。
- `FauxProvider`：脚本化响应队列，测试整个 agent 循环不需要真实 API key。
- API key 通过 `opts.APIKey` / `GetAPIKey func(provider string) string` 注入（对应 pi 的 `getApiKey`，支持过期 token 每次调用前重新取）。
- 两种 HTTP Provider 共用请求重试层：收到成功响应头之前，临时网络错误、429 和 5xx 默认最多重试 2 次，指数退避从 250ms 起步、上限 4s，并带半幅至全幅抖动；支持 context 取消和秒数/HTTP 日期形式的 `Retry-After`。服务端要求的等待超过退避上限时直接返回错误，不提前重试。每次尝试重建相同请求体和请求头。
- 构造函数保留原参数，并支持可选的 `WithHTTPClient`、`WithRetryPolicy` 和 `WithRequestLimiter`。注入 client 不会被修改，支持代理和自定义 Transport；默认不设覆盖整条 SSE 的 Client.Timeout，由调用 context 或注入的 client 控制。限流器可跨 Provider/并发调用共享，每次尝试包含重试都调用 `Wait(ctx)`，默认不限流。
- 成功响应返回后不再重试；SSE 读取错误、未完成的 EOF 和流内 error 返回错误，避免重放部分文本或工具调用。正常结束支持 OpenAI 的 `[DONE]`/finish_reason、Anthropic 的 message_stop/stop_reason，保留已有网关和缓存 usage 兼容性。

## 4. pkg/agent —— Agent 运行时（≈ pi-agent-core）

### 4.1 AgentMessage 与两段式管线

`AgentMessage` = `llm.Message` + `Custom bool` + `Payload any`（Go 没有 declaration merging，用显式字段支持自定义 UI 消息）。每次 LLM 调用前：

```
[]AgentMessage → TransformContext（可选：剪枝/注入外部上下文）
              → ConvertToLLM（过滤 custom 消息）
              → []llm.Message → LLM
```

两个函数都是 Agent 上可替换的字段，默认实现只过滤 `Custom`。

### 4.2 事件模型（事件驱动是整条架构的脊柱）

```go
type EventType string
const (
    EventAgentStart / EventTurnStart
    EventMessageStart / EventMessageUpdate / EventMessageEnd
    EventToolExecutionStart / EventToolExecutionUpdate / EventToolExecutionEnd
    EventTurnEnd / EventAgentEnd
)
```

`Agent.Subscribe(func(Event))`：监听器按注册顺序同步调用（pi 里是 await 序列；Go 里用同步调用 + 事件携带 `context.Context`）。`agent_end` 发出并等监听器返回后，run 才算 idle。`agent_end` 同时携带 `TerminationReason`，并写入 `State.TerminationReason`：completed / aborted / timeout / max_turns / output_limit / model_error / prompt_too_long / error。

### 4.3 双层循环（loop.go，忠实复刻 pi 的 runLoop）

```
外层 for：agent 本要停止时，排空 followUpQueue，非空则继续
  内层 for (hasMoreToolCalls || pendingMessages 非空)：
    0. turnCount++；若超过 MaxTurns，以 max_turns 发出 agent_end
    1. 注入 steering 消息（emit message_start/end）
    2. streamAssistantResponse：TransformContext → ConvertToLLM → StreamFn，
       select 消费 EventStream 与 ctx.Done，转成 message_* 事件
    3. stopReason 为 error/aborted → turn_end + 带原因的 agent_end，退出
    4. 有工具调用：
       - stopReason == "length"：整批判失败（pi 的截断保护：partial JSON
         挽救可能让残缺参数"看起来合法"，一律不执行，让模型重发）
       - 否则执行 → toolResult 入上下文
    5. turn_end → PrepareNextTurn（可换模型/思考级别）→ ShouldStopAfterTurn 钩子
    6. 无工具调用的 length 响应注入续写消息，最多恢复 3 次；
       耗尽后以 output_limit 结束
    7. 排空 steeringQueue 作为 pendingMessages
```

每次 assistant 响应计一个 turn，恢复请求也计入 MaxTurns。MaxDuration 通过 runCtx 同时约束 Provider 流和工具执行；ctx 取消时不等待 `stream.Result()`，而是用当前 partial 补齐 aborted 终态。Provider 的 413/context-length 类错误归为 prompt_too_long，其他 hook/转换错误归为 error。

### 4.4 工具执行三段管线

对应 pi 的 prepare → execute → finalize：

1. **Prepare**：按名查工具 → 校验参数（对照 JSON Schema 的 required/type 做基础校验）→ 权限评估 → `BeforeToolCall` 钩子（可 `Block`，可 `Terminate`）。
2. **Execute**：`tool.Execute(ctx, id, args, onUpdate)`；`onUpdate` 回调发 `tool_execution_update`（流式 partial result）。默认**并行**（goroutine + WaitGroup，结果按原顺序入上下文）；任一工具声明 `ExecutionModeSequential` 或全局 `ToolExecution = sequential` 时整批串行。
3. **Finalize**：`AfterToolCall` 钩子可改写 content/isError/terminate；随后统一执行工具输出截断或完整落盘。所有结果 `Terminate=true` 时终止循环。

`AgentTool` 接口：

```go
type AgentTool interface {
    Name() string
    Schema() llm.Tool            // name/description/parameters
    ExecutionMode() ExecutionMode
    Execute(ctx context.Context, callID string, args json.RawMessage,
            onUpdate func(ToolResult)) (ToolResult, error)
}
```

### 4.5 权限评估（pkg/permission）

- `Options.Permission` 接收独立 `permission.Evaluator`；为 nil 时普通工具不评估，保持已有库嵌入行为；实现 `ToolCallPreparer` 的工作区工具使用默认策略而非跳过检查，默认 CLI 显式启用 `permission.Policy`。
- `Policy` 支持 default / acceptEdits / plan / dontAsk / bypassPermissions，以及 `*` 或精确工具名规则；deny 规则和工具硬拒绝优先于所有自动允许。
- Bash 还支持命令正则：allow 自动锚定完整命令，且含 shell 控制符、重定向或命令替换的命令不会自动放行；deny 使用部分匹配并优先于 bypass/`-yes`/审批。
- `SetMode` / `CurrentMode` 支持运行时模式切换；`Evaluate` 在读锁下快照配置后释放锁，审批等待期间不持锁，可与 TUI 和工具 goroutine 并发执行。
- 工具通过可选接口提供只读属性和权限自检；未提供的自定义工具按非只读、需要审批处理。只读工具自动允许，write/edit/bash 默认进入宿主审批。
- 权限拒绝、审批中止和 context 取消都生成配对的 error ToolResult；权限评估先于 `BeforeToolCall`，审批器只负责 allow/deny，异常或空决策一律 fail closed。
- 权限层不是 sandbox；高权限执行仍建议使用容器或受限用户。
- 默认 CLI 使用 `tools.Workspace.CodingTools()`，根目录由 `-workspace`（默认启动目录）确定。文件调用在权限评估前通过 `ToolCallPreparer` 绑定解析后的目标和参数，读取外部目标返回 `RequiredApproval`；此决策不能被只读、allow 规则或 bypass 自动允许，dontAsk/print 拒绝。外部写入直接拒绝，内部写入遵循原有模式。额外目录的审批仅作用于当前调用，不缓存为全局授权。
- Unix 文件访问逐级使用目录句柄和 `O_NOFOLLOW`，安全打开时不重新跟随链接；父目录创建也经相同路径。递归搜索在授权树中安全打开每个目录和文件，跳过逃逸链接。特殊文件拒绝读取/写入，硬链接拒绝写入。其他平台明确报错，不退回不安全检查；bash 仅设置工作目录，不提供进程沙箱。底层无 Workspace 的工具构造保留兼容行为。

### 4.6 状态与并发

- 单活动 run：`Prompt()` 进行中再调用返回错误，引导用 `Steer()` / `FollowUp()`。
- 双队列 `PendingMessageQueue`，`QueueMode = "all" | "one-at-a-time"`。
- `Abort()` 通过 `context.CancelFunc`；`WaitForIdle()` 用 `chan struct{}`。
- `MaxDuration > 0` 时 runCtx 由 `context.WithTimeout` 创建，否则由 `context.WithCancel` 创建；`beginRun`/`Reset` 会清空上一次的终止原因。
- 内部状态用 `sync.Mutex` 保护（Go 没有单线程事件循环假设）。

## 5. pkg/harness —— 会话与压缩（≈ harness + session-manager）

### 5.1 树状会话（append-only JSONL）

忠实复刻 pi 的 `SessionManager`：

- 每行一个 entry：`{id, parentId, timestamp, type, payload}`。type 包括 `message`、`model_change`、`thinking_level_change`、`compaction`、`branch_summary`、`custom`。
- entry 通过 `parentId` 构成**树**；`leafID` 指向当前分支末端。
- `Branch(fromID)`：把 leaf 移回历史节点，之后 append 的 entry 自然形成新分支（旧路径不删除）。
- `BranchWithSummary(fromID, summary)`：分叉同时给被放弃的路径留一条摘要 entry。
- 重建上下文：从 leaf 沿 parentId 走到根，反转得路径，途中遇到 `compaction` entry 时用其 summary 替代被压缩区段。

### 5.2 Compaction

上下文接近窗口上限时，调 LLM 对旧消息生成摘要，append `CompactionEntry{summary, firstKeptEntryID, tokensBefore}`。对应 pi 的 `transformContext` + compaction entry 的组合。

重建上下文时，`firstKeptEntryID` 定位未压缩尾部，最终序列为 `summary + kept messages`；原始 entry 仍全部保留在 JSONL 中。若保留边界落在 `toolResult` 前，则将该结果及其之前的调用一并压缩，避免恢复出孤立的工具结果。

### 5.3 Harness 组装

`Harness` 持有 `*agent.Agent` + `*Session`：订阅 agent 事件自动落盘；提供 `Prompt / Branch / Compact / Resume`；崩溃恢复 = 重新读 JSONL 重放到 leaf。

### 5.4 Skills（对应 pi 的 skills 资源）

- **发现**：`LoadSkills(root, source)` 扫描 root 下含 `SKILL.md` 的目录（任意深度），解析 frontmatter（`name`/`description`，长度上限 64/1024，与 pi 一致）；尊重扫描根的 `.gitignore`/`.ignore`（简化语义：否定、目录限定、锚定、glob），点开头目录默认跳过；解析失败的技能静默跳过。
- **合并**：`MergeSkills` 按来源合并，后者覆盖同名前者（project > global）。
- **注入**：`Harness.ApplySkills(globalDir, projectDir)` 把技能清单渲染为系统提示词段落（`FormatForSystemPrompt`）；模型按需用 `read` 工具读 SKILL.md 全文——与 pi 的按需加载路径一致。

## 6. pkg/tools —— 内置编码工具

全部实现 `agent.AgentTool`：

| 工具 | 说明 | 关键实现 |
|---|---|---|
| `bash` | 执行 shell 命令 | `os/exec` + 超时 + 进程组清理；完整输出交给 Agent 统一治理 |
| `read` | 读文件（offset/limit，行号前缀） | `bufio.Scanner` |
| `write` | 写文件 | 经 `FileMutationQueue`（全局 mutex 串行化写操作，对应 pi 的 file-mutation-queue） |
| `edit` | 精确字符串替换（old_string 唯一性校验） | 同 pi 的 edit 语义 |
| `multi_edit` | 同文件多组顺序替换 | 全部成功后才写盘，全程持有 FileMutationQueue |
| `ls` | 列目录 | `os.ReadDir` |
| `grep` | 正则搜索文件内容 | `regexp` + `filepath.WalkDir` + .gitignore 简化支持 |
| `glob` | 按 `*`、`?`、`**` 查找文件 | rune-safe 模式转 regexp + 排序 + max_results |

最终 ToolResult 在 `AfterToolCall` 后统一按字符数捕获：未配置 `ToolOutputDir` 时进行 rune-safe 头尾截断；配置后以 0600 临时文件保存完整文本，模型仅收到头部、省略说明和路径。`MaxToolOutputChars` 默认 30000，多个文本块合计计算，非文本块保留。

## 7. pkg/tui + cmd/goagent —— 终端客户端

- **TUI 模式（默认）**：Bubble Tea 全屏应用，Bubbles viewport/textarea 分别负责历史滚动与多行输入，Lip Gloss 负责样式和自适应布局。`Agent.Subscribe` 的生命周期事件通过 `Program.Send` 转入 UI 更新循环，assistant 流式内容与并行工具状态按事件归并。
- **权限审批**：TUI Approver 通过 `Program.Send` 提交请求并等待响应 channel，Model 排队处理并行工具审批。审批以独立面板呈现，始终展示完整（美化后）的参数：默认展开，可 Ctrl+O 折叠为带字符数的摘要行，展开时 PgUp/PgDn 在参数区内滚动；不存在"仅展示前缀"的审批语义。选择是显式两步操作：↑/↓/Tab 或 1/2 移动选中项（默认选中"拒绝"），Enter 确认，单键 `y`/`n` 不再生效；Esc/Ctrl+C 拒绝未决请求并中止 run。`Shift+Tab` 在 default / acceptEdits / plan 间循环并更新状态栏，bypass/dontAsk 不进入快捷键循环。REPL 与普通输入共用同一个 `bufio.Reader`，打印完整参数后要求输入 `1`（允许）或 `2`（拒绝），其他输入重新询问；print 的 Approver 直接拒绝，不读取 stdin。
- **运行轨迹**：thinking 与工具调用/输出带活动状态。`auto` 模式下活动中展开、结束后折叠并保留摘要；`expanded`/`collapsed` 手动控制全部轨迹，Ctrl+O 切换，`/trace auto|expand|collapse` 精确设置。
- **Token 用量**：assistant message_end 时累计 provider usage，状态栏显示最近 input/output、当前分支累计 total 和按 token 加权的缓存命中率；`/usage` 展示最近/累计命中率及缓存、成本明细。resume/branch/compact 时从消息重建，避免重复计数。
- **异步操作**：prompt、compaction 和用户 bash 都作为 `tea.Cmd` 执行；Esc/Ctrl+C 通过 context 与 `Agent.Abort()` 中止。Enter 发送，Alt+Enter 等组合键换行，PgUp/PgDn/鼠标滚轮浏览历史。
- **REPL 回退**：`goagent -repl` 保留原 stdlib `bufio` 行式界面，适合不需要全屏控制的环境。
- **终止状态**：TUI 在 agent_end 后显示非 completed 原因；REPL/print 通过 printSubscriber 输出原因，print 按 completed=0、timeout=124、aborted=130、其他=1 退出。
- **print 模式**：`goagent -p "prompt"`，流式输出文本，工具调用打印摘要，退出码反映 TerminationReason。
- 会话命令：`/help`、`/skills`、`/branch <id>`、`/compact`、`/trace`、`/tree`、`/resume`、`/usage`、`/quit`；`!cmd` 执行 bash 并在当前界面显示输出，不注入模型上下文。
- 配置：环境变量 `OPENAI_API_KEY`/`OPENAI_BASE_URL`/`GOAGENT_MODEL`、`ANTHROPIC_API_KEY`；会话文件默认 `~/.goagent/sessions/`；权限由 `-permission`、`-yes`、`-allow`、`-deny`、`-allow-command`、`-deny-command` 控制；预算由 `-max-turns`（默认 50）和 `-max-duration`（默认关闭）控制；输出由 `-tool-output-dir`（默认 `~/.goagent/tool-output`）和 `-max-tool-output`（默认 30000）控制。

## 8. 与 pi 的差异（刻意决策）

1. **无动态扩展加载**：pi 的 TS 扩展系统在 Go 里用「钩子字段 + 工具注册接口」替代，能力面一致（事件订阅、before/after tool、注册工具/命令），牺牲运行时热插拔。
2. **使用现成 TUI 框架**：pi-tui 是自研差分渲染框架，goagent 使用 Charm 生态的 Bubble Tea/Bubbles/Lip Gloss，核心 agent/harness 不依赖这些 UI 包。
3. **权限控制但无 sandbox**：独立权限层提供模式、规则和审批；进程与文件系统不做强制隔离，高权限运行仍建议容器化。
4. **无 RPC/server**：留作路线图（pi 的这一层本身也标注 experimental）。

## 9. 开发计划

| 阶段 | 内容 | 验收 |
|---|---|---|
| 1 | `pkg/llm`：类型、EventStream、Registry、Faux、OpenAI、Anthropic | 单测：faux 流式事件序列 |
| 2 | `pkg/agent`：loop、双队列、工具三段管线、截断保护 | 单测：脚本化工具调用循环、steering 注入、abort |
| 3 | `pkg/harness`：JSONL 会话树、branch、compaction | 单测：分支/恢复/压缩重建 |
| 4 | `pkg/tools` + `cmd/goagent` | 单测：edit/write/grep；`go build ./...` 通过 |
| 5 | 集成演示：faux provider 跑通一次完整编码会话 | `go test ./...` 全绿 |
