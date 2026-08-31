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
| `pi-tui` | 简易行式 REPL（stdlib `bufio`） | 保持零外部依赖；TUI 留作后续路线 |
| RPC / server / client | 路线图 | 暂不进内核 |

关键约束：**只用 Go 标准库**（`net/http` + SSE 手工解析、`encoding/json`、`os/exec`），保证 `go build` 零依赖、可离线复现。

## 2. 分层结构

```
┌──────────────────────────────────────────────────┐
│ cmd/goagent        CLI（interactive / print）      │
├──────────────────────────────────────────────────┤
│ pkg/harness        会话树（JSONL）、分支、压缩      │
├──────────────────────────────────────────────────┤
│ pkg/agent          Agent 状态机 + agent loop       │
│  ├─ Agent            状态、事件订阅、双队列         │
│  └─ loop             runLoop：外层 follow-up /     │
│                      内层 tool-call 循环           │
├──────────────────────────────────────────────────┤
│ pkg/llm            Provider 抽象 + 流式事件流       │
│  ├─ Provider 接口    OpenAI 兼容 / Anthropic       │
│  ├─ EventStream      channel 化的事件流            │
│  └─ FauxProvider     脚本化假模型（测试用）         │
├──────────────────────────────────────────────────┤
│ pkg/tools          bash/read/write/edit/ls/grep    │
└──────────────────────────────────────────────────┘
```

依赖方向严格自上而下：`agent` 不认识具体 provider（只认 `llm.StreamFn`），`harness` 不认识 CLI，`llm` 不认识 agent。

## 3. pkg/llm —— 统一 LLM 层（≈ pi-ai）

### 3.1 消息模型

pi 区分 `AgentMessage`（宽）与 LLM `Message`（窄）。Go 版同样处理：

- `llm.Message`：`role(user/assistant/toolResult)` + `[]Content`。
- `Content` 用接口 + 具体类型：`TextContent`、`ThinkingContent`、`ToolCallContent`（含 id/name/arguments JSON）、`ImageContent`（base64）。
- `AssistantMessage` 附带 `Usage`（input/output/cacheRead/cacheWrite + cost）、`StopReason`（`stop/length/toolUse/error/aborted`）。
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
- 内置 `OpenAIProvider`（兼容 OpenAI / DeepSeek / 各类网关，SSE 流式 + tool_calls 增量合并）与 `AnthropicProvider`（SSE，`content_block_*` 事件映射）。
- `FauxProvider`：脚本化响应队列，测试整个 agent 循环不需要真实 API key。
- API key 通过 `opts.APIKey` / `GetAPIKey func(provider string) string` 注入（对应 pi 的 `getApiKey`，支持过期 token 每次调用前重新取）。

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

`Agent.Subscribe(func(Event))`：监听器按注册顺序同步调用（pi 里是 await 序列；Go 里用同步调用 + 事件携带 `context.Context`）。`agent_end` 发出并等监听器返回后，run 才算 idle。

### 4.3 双层循环（loop.go，忠实复刻 pi 的 runLoop）

```
外层 for：agent 本要停止时，排空 followUpQueue，非空则继续
  内层 for (hasMoreToolCalls || pendingMessages 非空)：
    1. 注入 steering 消息（emit message_start/end）
    2. streamAssistantResponse：TransformContext → ConvertToLLM → StreamFn，
       消费 EventStream 转成 message_* 事件
    3. stopReason 为 error/aborted → turn_end + agent_end，退出
    4. 有工具调用：
       - stopReason == "length"：整批判失败（pi 的截断保护：partial JSON
         挽救可能让残缺参数"看起来合法"，一律不执行，让模型重发）
       - 否则执行 → toolResult 入上下文
    5. turn_end → PrepareNextTurn（可换模型/思考级别）→ ShouldStopAfterTurn 钩子
    6. 排空 steeringQueue 作为 pendingMessages
```

### 4.4 工具执行三段管线

对应 pi 的 prepare → execute → finalize：

1. **Prepare**：按名查工具 → 校验参数（对照 JSON Schema 的 required/type 做基础校验）→ `BeforeToolCall` 钩子（可 `Block`，可 `Terminate`）。
2. **Execute**：`tool.Execute(ctx, id, args, onUpdate)`；`onUpdate` 回调发 `tool_execution_update`（流式 partial result）。默认**并行**（goroutine + WaitGroup，结果按原顺序入上下文）；任一工具声明 `ExecutionModeSequential` 或全局 `ToolExecution = sequential` 时整批串行。
3. **Finalize**：`AfterToolCall` 钩子可改写 content/isError/terminate。所有结果 `Terminate=true` 时终止循环。

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

### 4.5 状态与并发

- 单活动 run：`Prompt()` 进行中再调用返回错误，引导用 `Steer()` / `FollowUp()`。
- 双队列 `PendingMessageQueue`，`QueueMode = "all" | "one-at-a-time"`。
- `Abort()` 通过 `context.CancelFunc`；`WaitForIdle()` 用 `chan struct{}`。
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

### 5.3 Harness 组装

`Harness` 持有 `*agent.Agent` + `*Session`：订阅 agent 事件自动落盘；提供 `Prompt / Branch / Compact / Resume`；崩溃恢复 = 重新读 JSONL 重放到 leaf。

## 6. pkg/tools —— 内置编码工具

对齐 pi 的最小工具集，全部实现 `agent.AgentTool`：

| 工具 | 说明 | 关键实现 |
|---|---|---|
| `bash` | 执行 shell 命令 | `os/exec` + 超时 + 输出截断（头尾保留） |
| `read` | 读文件（offset/limit，行号前缀） | `bufio.Scanner` |
| `write` | 写文件 | 经 `FileMutationQueue`（全局 mutex 串行化写操作，对应 pi 的 file-mutation-queue） |
| `edit` | 精确字符串替换（old_string 唯一性校验） | 同 pi 的 edit 语义 |
| `ls` | 列目录 | `os.ReadDir` |
| `grep` | 正则搜索文件内容 | `regexp` + `filepath.WalkDir` + .gitignore 简化支持 |

## 7. cmd/goagent —— CLI

- **print 模式**：`goagent -p "prompt"`，流式输出文本，工具调用打印摘要，退出码反映 stopReason。
- **interactive 模式**：行式 REPL（`bufio.Reader`），命令：
  - `/model`、`/branch <id>`、`/compact`、`/tree`、`/quit`
  - `!cmd` 直接把 bash 输出注入上下文（对应 pi 的 user_bash）
- 配置：环境变量 `OPENAI_API_KEY`/`OPENAI_BASE_URL`/`GOAGENT_MODEL`、`ANTHROPIC_API_KEY`；会话文件默认 `~/.goagent/sessions/`。

## 8. 与 pi 的差异（刻意决策）

1. **无动态扩展加载**：pi 的 TS 扩展系统在 Go 里用「钩子字段 + 工具注册接口」替代，能力面一致（事件订阅、before/after tool、注册工具/命令），牺牲运行时热插拔。
2. **无内置 TUI 框架**：pi-tui 是上千行的差分渲染框架，goagent 一期只做行式 REPL。
3. **无权限系统**：与 pi 一致，不做 sandbox，建议容器化运行。
4. **无 RPC/server**：留作路线图（pi 的这一层本身也标注 experimental）。

## 9. 开发计划

| 阶段 | 内容 | 验收 |
|---|---|---|
| 1 | `pkg/llm`：类型、EventStream、Registry、Faux、OpenAI、Anthropic | 单测：faux 流式事件序列 |
| 2 | `pkg/agent`：loop、双队列、工具三段管线、截断保护 | 单测：脚本化工具调用循环、steering 注入、abort |
| 3 | `pkg/harness`：JSONL 会话树、branch、compaction | 单测：分支/恢复/压缩重建 |
| 4 | `pkg/tools` + `cmd/goagent` | 单测：edit/write/grep；`go build ./...` 通过 |
| 5 | 集成演示：faux provider 跑通一次完整编码会话 | `go test ./...` 全绿 |
