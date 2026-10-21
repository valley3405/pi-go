# TODO：借鉴 Norma 的优化路线

本文档根据本项目与 [Norma](https://github.com/Autumn-27/Norma) 的源码对比整理，目标不是追求功能数量对齐，而是在保留本项目优势的前提下，优先修复正确性问题，再补安全、调度和长任务能力。

## 优化原则

- 保留现有优势：Bubble Tea TUI、树状会话与分支、`Steer`/`FollowUp` 双队列、多监听者事件模型、Provider Registry、图片消息。
- 优先提升正确性、安全性和运行可控性，再增加 MCP、多 Agent 等外围能力。
- 核心 `llm`、`agent`、`harness`、`tools` 尽量维持标准库实现；新增依赖必须有明确收益。
- 不用单一巨型配置对象替代现有分层；新能力优先通过独立包、接口和组合入口接入。
- 每项改动都应包含单元测试，并保持 `go build ./...`、`go test ./...` 和 FauxProvider 端到端演示可用。

## P0：正确性与安全基线

### 1. 修复 compaction 实际未裁剪上下文的问题（已完成）

位置：`pkg/harness/compaction.go`、`pkg/harness/session.go`、`pkg/harness/session_test.go`

原问题：`Compact()` 只追加摘要，`Session.Messages()` 仍保留全部旧消息，且未使用 `FirstKeptEntryID`。现已按“摘要 + 保留尾部”重建上下文。

- [x] 正确定义并写入第一个未压缩 entry 的 ID，不再直接使用压缩前的 `LeafID()`。
- [x] 重建当前分支时，将上下文恢复为“压缩摘要 + 保留的最近消息”，旧消息仅保留在 append-only JSONL 中，不再进入 LLM 上下文。
- [x] 处理连续多次压缩、压缩后 Branch、重新 OpenSession/Resume 等路径。
- [x] 选择压缩边界时避免产生孤立的 `toolCall` 或 `toolResult`。
- [x] 同步修正 `DESIGN.md`、相关注释和现有测试预期。

验收：例如 10 条消息、保留最近 4 条时，重建结果为 1 条摘要加 4 条最近消息；重复压缩和分支恢复结果一致，重建后的估算 token 数确实下降。

### 2. 增加独立权限层，并接入默认 CLI（已完成）

位置：`pkg/permission`、`pkg/agent/loop.go`、`pkg/tools`、`cmd/goagent`、`internal/tui`

参考：Norma 的 [`permission`](https://github.com/Autumn-27/Norma/blob/main/permission/permission.go) 和 fail-closed 工具默认值。

- [x] 定义 `allow / deny / ask` 决策及 default、acceptEdits、auto、plan、dontAsk、bypassPermissions 模式。
- [x] 支持 `*` 和精确工具名 allow/deny 规则，保证 deny 优先级高于 allow 和 bypass；参数级规则暂未支持。
- [x] 为工具补充只读和权限自检元数据；未声明安全属性的自定义工具采用保守默认值。并发安全元数据由第 3 项继续完善。
- [x] 明确执行顺序：参数校验 → 权限评估 → `BeforeToolCall` → Execute → `AfterToolCall`。
- [x] TUI/REPL 提供工具审批交互；print 不等待 stdin，通过 `-yes`、`-allow` 或相应权限模式显式放权。
- [x] TUI 支持 `Shift+Tab` 在 default、acceptEdits、auto、plan 间循环切换，并在状态栏显示当前模式。
- [x] 权限拒绝和运行中止时仍为每个工具调用生成配对结果，避免破坏下一轮 LLM 消息。
- [x] 文档明确：权限系统不是容器沙箱，高权限运行仍建议使用容器或受限用户。

验收：权限模式与规则组合有表驱动测试；写文件、Bash、只读工具分别覆盖允许、拒绝、审批和 bypass；默认 CLI 不再无提示执行未授权写操作。

### 2.1 增加 Bash 命令级权限规则（已完成）

位置：`pkg/permission`、`cmd/goagent`、`pkg/agent`

- [x] `Policy` 增加 AllowedCommands / DeniedCommands，仅作用于 Bash 的 command 参数。
- [x] CLI 新增可重复 `-allow-command` / `-deny-command`，正则包含逗号时也不会被拆分。
- [x] allow 命令规则锚定完整命令，含 shell 控制符、重定向或命令替换的命令仍需审批。
- [x] deny 命令规则按部分匹配，优先于 `-yes`、bypass、工具 allow 规则和审批。
- [x] plan 仍禁止 Bash；dontAsk/print 下仅 allow 命中的命令自动执行。
- [x] 非法规则启动时报错，库调用中 fail closed。

### 2.2 增加必要时询问的 auto 权限模式（已完成）

位置：`pkg/permission`、`pkg/tools/workspace_test.go`、`internal/tui`、`cmd/goagent`

- [x] 新增 `-permission auto` 和 `Policy.WorkingDir`。
- [x] 只读工具、工作区内写工具和常规开发命令自动执行。
- [x] 工作区内删除/移动/修改（包括工作区位于 `/tmp` 时）自动执行；越界路径进入审批。
- [x] 网络、提权、系统/进程控制、包安装、Git 写入、未知命令仍需询问。
- [x] TUI `Shift+Tab` 循环包含 auto，RequiredApproval、deny 和 deny-command 仍然优先。

## P1：核心运行能力

### 3. 将整批串行调度改为细粒度工具调度

位置：`pkg/agent/types.go`、`pkg/agent/loop.go`、`pkg/agent/agent_test.go`

参考：Norma 的 [`streamExec`](https://github.com/Autumn-27/Norma/blob/main/harness/streaming.go)。

- [ ] 用 P0 的工具安全元数据替代“一个 sequential 工具导致整批串行”的粗粒度策略。
- [ ] 只读且并发安全的工具可并行；写操作或未知工具需要独占执行，并阻止其后的不安全工具提前启动。
- [ ] 增加 `MaxConcurrency`，防止模型一次产生过多 goroutine/子进程。
- [ ] 工具完成事件可以乱序，但写回 LLM 上下文的 `toolResult` 必须保持原调用顺序。
- [ ] 保留现有全局 `ExecutionMode` 的兼容迁移路径，避免一次性破坏自定义工具。

验收：混合只读/写入调用有调度顺序测试；结果顺序稳定；并发上限有效；`go test -race ./pkg/agent ./pkg/tools` 通过。

### 4. 把上下文管理接入每次 LLM 请求

位置：`pkg/harness/harness.go`、`pkg/harness/compaction.go`、`pkg/agent/loop.go`、`pkg/llm`

参考：Norma 的 [`compaction`](https://github.com/Autumn-27/Norma/blob/main/compaction/compaction.go)。

- [ ] 将自动压缩从仅在 `Harness.Prompt()` 前检查，扩展为工具循环中的每次 LLM 请求前检查。
- [ ] 优先使用上一轮 Provider 返回的真实 usage，真实数据缺失时再回退到字符估算。
- [ ] 为上下文窗口预留模型输出和下一轮工具结果空间，而不是只使用固定的 75% 阈值。
- [ ] 增加 MicroCompact：优先清理可重新读取的旧工具结果，保留最近若干条。
- [ ] 为 Provider 增加可识别的上下文溢出错误，支持一次 reactive compact 后重试。
- [ ] 连续摘要失败达到阈值后触发熔断，避免每次请求都重复调用摘要模型。
- [ ] 防止摘要调用自身再次触发自动压缩。

验收：多轮工具调用中途越阈值也能压缩；413/context-length 可恢复；摘要失败不会形成递归或无限重试；压缩前后工具调用配对完整。

### 5. 增加运行预算和明确的终止原因（已完成）

位置：`pkg/agent/agent.go`、`pkg/agent/loop.go`、`pkg/agent/types.go`、`cmd/goagent`

- [x] 增加 `MaxTurns` 和 `MaxDuration`；每次 assistant/LLM 响应计一个 turn，包含工具后续回合和恢复请求。
- [x] 在事件和状态中区分 completed、aborted、max_turns、timeout、output_limit、model_error、prompt_too_long、error。
- [x] 超时同时覆盖模型流和工具执行；流等待监听 ctx，取消时补齐终态和工具结果，内置 Bash 继续清理进程组。
- [x] 对无工具调用的 `StopLength` 增加最多 3 次续写恢复，耗尽后以 output_limit 结束。
- [x] print 按原因返回 0/1/124/130，TUI 和 REPL 展示明确的结束原因。

验收：持续调用工具的脚本模型不能无限运行；流式响应和工具执行两种场景下均能按时停止；恢复次数有硬上限，事件终态完整。

### 6. 统一工具输出治理，并补高频编码工具（已完成）

位置：`pkg/agent/loop.go`、`pkg/agent/output.go`、`pkg/tools`

- [x] 在 finalize 阶段按字符数统一限制工具输出，内置、自定义和钩子改写后的结果都经过同一逻辑。
- [x] 支持可选的 `ToolOutputDir`：超限完整输出以 0600 文件落盘，模型接收头部、截断说明和文件路径；失败时回退到头尾截断。
- [x] Bash 不再预截断，保留超时和进程组清理；无落盘时采用 rune-safe 头尾截断。
- [x] 增加 `glob` 和 `multi_edit`，减少模型用 Bash 替代专用工具的需求；acceptEdits 覆盖 multi_edit。
- [x] TodoWrite 经评估移至后续候选，避免本次同时扩大任务状态和 UI 范围。

验收：内置工具与自定义工具经过同一输出上限；落盘文件内容完整；新增工具有临时目录集成测试，写操作仍受权限和调度约束。

### 7. 提升 Provider 的网络健壮性（已完成）

位置：`pkg/llm/openai.go`、`pkg/llm/anthropic.go`、`pkg/llm/types.go`

参考：Norma 的 [`Provider`](https://github.com/Autumn-27/Norma/blob/main/llm/provider.go)。

- [x] 对收到成功响应头之前的超时、连接重置、429 和 5xx 增加有界指数退避重试；默认最多重试 2 次，250ms 起步、4s 上限并加入抖动，支持取消和 `Retry-After`。
- [x] 收到成功响应头后不再自动重放请求；SSE 提前 EOF、读错误和流内 error 均报错，兼容仅提供 finish/stop reason 的正常结束。
- [x] 通过构造选项注入 `http.Client`，测试覆盖代理、超时和自定义 Transport；现有构造调用保持兼容。
- [x] 提供可跨 Provider 共享的 `RequestLimiter` 注入接口，每次 HTTP 尝试（包含重试）都经过限流；暂不内置固定厂商速率，默认不限流，并发共享与限流拒绝有测试覆盖。
- [x] 保持现有 Registry、逐次调用的 API Key/Headers、图片消息和缓存 usage 行为不变。

验收：使用 `httptest` 覆盖重试、退避、429/5xx、SSE 中途断开和不重放场景；现有 OpenAI/Anthropic 测试保持通过。

## P2：增强交互与复杂任务能力

### 8. 评估模型流式输出与工具执行重叠

位置：`pkg/agent/loop.go`、`pkg/llm/stream.go`

- [ ] 在完整收到单个 `toolcall_end` 后立即校验并调度该工具，不必等待整个 assistant 响应结束。
- [ ] 先定义 `StopLength` 的新语义：未启动的调用应失败并配对结果；已经启动的写操作无法回滚，必须在文档中明确。
- [ ] 权限审批期间继续缓存模型事件，避免 UI 状态或工具调用顺序错乱。
- [ ] 若无法同时保留截断保护和副作用安全，宁可保留当前“完整响应后执行”的行为。

验收：工具参数未完整前绝不执行；文本流、工具事件和最终结果顺序一致；截断、拒绝、Abort 场景均无无配对调用。

### 9. 让 Skills 成为显式调用并能在压缩后存活

位置：`pkg/harness/skills.go`、`pkg/harness/compaction.go`、`pkg/agent`

- [x] 保留当前“系统提示词放清单、按需加载正文”的上下文节省方式。
- [x] 可增加 `Skill` 工具，将技能正文作为显式 guidance 消息注入，而不是依赖模型自行 `read`。（已实现为 `pkg/harness/skill_tool.go` 的只读 `skill` 工具）
- [x] 为技能调用消息增加可识别元数据，压缩时按技能保留最近一次有效调用。（`SkillResultDetails` 写入消息 `Payload`）
- [x] 压缩摘要后按单项和总 token 预算重新注入必要技能正文。（**限制**：只重注入全局最近一次有效调用的一个技能，单项预算 8000 字符即总预算；压缩前若加载过多个技能，仅最后一个存活。如需多技能并存需改为按技能分组 + 独立总预算）
- [x] 暂不为了完整 YAML 兼容性随意增加核心依赖，先明确现有 frontmatter 的支持边界。（零依赖 YAML 子集，见 skills.go 注释）

验收：技能只加载一次正文；压缩后关键技能指令仍存在（限最近一次加载的技能）；未调用技能不占用正文上下文；全局/项目覆盖规则不变。

### 10. 增加后台任务生命周期管理

位置：可新增 `pkg/tasks`；接入 `pkg/tools/bash.go`、`pkg/agent`、`internal/tui`

- [ ] 按会话管理后台任务，提供 TaskList、TaskOutput、TaskStop。
- [ ] 在回合边界把任务完成通知注入上下文，避免忙等。
- [ ] Session Close、Abort 和进程退出时清理任务及临时输出，复用现有进程组处理能力。
- [ ] 后台任务继承权限和工作目录约束，不成为绕过审批的通道。

验收：任务可查询、停止和恢复输出；会话关闭后无残留进程；完成通知不会重复注入。

### 11. 在权限与调度稳定后增加 Subagent

位置：可新增 `pkg/subagent`；扩展 `pkg/harness/session.go`

- [ ] 子 Agent 使用独立消息上下文、系统提示词和可裁剪工具集。
- [ ] 默认继承或收紧父 Agent 权限，设置最大递归深度、turn、时间和并发限制。
- [ ] JSONL 会话增加 `agentID`/sidechain 信息，主会话与子 Agent usage 分开统计。
- [ ] 先提供单个 `Agent` 委派工具，再评估 coordinator/`RunParallel`，避免一次性引入复杂编排。

验收：递归深度和并发有硬上限；子 Agent 不能扩大权限；父会话 Resume 不混入子 Agent 私有上下文；sidechain 可独立审计。

## 后续候选：先单独设计，不进入近期核心

- [ ] **TodoWrite**：让长任务计划可被 TUI 和宿主观察，先定义任务状态模型与 UI。
- [ ] **Plan Mode**：基于只读权限模式、计划审批和 TUI 流程实现，不在 Agent loop 中另建一套平行状态机。
- [ ] **MCP**：优先 stdio JSON-RPC client；结合工具命名空间、权限规则和 deferred schema，防止工具清单无限膨胀。
- [ ] **长期 Memory**：与树状会话分离，明确写入、召回、过期和用户删除策略，避免把会话历史直接当作长期记忆。
- [ ] **Deferred Tools**：在 MCP 或大量自定义工具出现后再做，按需提供 Schema 并控制可调用集合。
- [ ] **OpenAI Responses API / 非流式 Provider**：根据实际模型需求增加，复用统一消息和事件模型，不复制 Agent loop。
- [x] **SDK 发布整理**：若对外作为库发布，将 `module goagent` 调整为真实仓库路径，稳定公开 API，并同步 README、DESIGN 和示例。（已完成：模块路径 `github.com/valley3405/pi-go`，`pkg/tui` 收进 `internal/tui`，公开库面为 `pkg/llm`、`pkg/agent`、`pkg/permission`、`pkg/tools`、`pkg/harness`）

## 不建议直接照搬的部分

- 不用 `iter.Seq2` 全面替换当前事件发布订阅；现有模型更适合 TUI、会话落盘和用量统计并行监听。
- 不用线性 transcript 替换树状会话；Norma 的 subagent sidechain 可以借鉴，但 Branch/tree 应继续保留。
- 不为了功能对齐一次性加入 Memory、MCP、Coordinator 等全部能力。
- 不复制 Norma 巨型 `Options` 风格；优先小型选项、独立组件和显式组装。
- 不在权限层未稳定前开放 Web、后台任务或更多高副作用工具。

## 推荐实施顺序

1. compaction 正确性修复。
2. 权限元数据与权限管线。
3. 细粒度调度、运行预算和统一输出治理。
4. 每次 LLM 请求前的上下文管理及 reactive recovery。
5. Provider 健壮性与高频编码工具。
6. ~~Skills 压缩存活~~（已完成，限单技能）、流式工具重叠。
7. 后台任务、Subagent。
8. 按实际需求评估 Plan、MCP、Memory 等后续能力。

## 总体验收

- [ ] `go build ./...` 通过。
- [ ] `go test ./...` 通过，核心并发包通过 `go test -race`。
- [ ] `go run ./examples/fauxdemo` 无需 API Key 完成端到端流程。
- [ ] TUI/REPL/print 三种模式均能正确处理审批、中止、超时和恢复。
- [ ] 长时间工具会话的上下文有上限，Resume/Branch 后消息及 usage 不重复、不丢失。
- [ ] 新能力均不能绕过权限、并发和进程清理约束。
