package agent

import (
	"context"
	"fmt"
	"sync"

	"goagent/pkg/llm"
)

// eventSink 是循环内部的事件出口。
type eventSink func(Event) error

// runAgentLoop 以新 prompt 启动循环（对应 pi 的 runAgentLoop）。
func runAgentLoop(ctx context.Context, prompts []AgentMessage, agentCtx Context, cfg loopConfig, streamFn llm.StreamFn, emit eventSink) ([]AgentMessage, error) {
	newMessages := append([]AgentMessage{}, prompts...)
	curCtx := Context{
		SystemPrompt: agentCtx.SystemPrompt,
		Messages:     append(append([]AgentMessage{}, agentCtx.Messages...), prompts...),
		Tools:        agentCtx.Tools,
	}

	if err := emit(Event{Type: EventAgentStart}); err != nil {
		return nil, err
	}
	if err := emit(Event{Type: EventTurnStart}); err != nil {
		return nil, err
	}
	for _, p := range prompts {
		pm := p
		if err := emit(Event{Type: EventMessageStart, Message: &pm}); err != nil {
			return nil, err
		}
		if err := emit(Event{Type: EventMessageEnd, Message: &pm}); err != nil {
			return nil, err
		}
	}
	err := runLoop(ctx, &curCtx, &newMessages, cfg, streamFn, emit)
	return newMessages, err
}

// runLoop 是双层主循环，忠实复刻 pi 的 runLoop：
// 外层在 agent 本要停止时消费 follow-up 队列；
// 内层处理工具调用与 steering 消息，直到没有更多工作。
func runLoop(ctx context.Context, curCtx *Context, newMessages *[]AgentMessage, cfg loopConfig, streamFn llm.StreamFn, emit eventSink) error {
	firstTurn := true
	var pending []AgentMessage
	if cfg.getSteering != nil {
		pending = cfg.getSteering(ctx)
	}

	for {
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pending) > 0 {
			if !firstTurn {
				if err := emit(Event{Type: EventTurnStart}); err != nil {
					return err
				}
			} else {
				firstTurn = false
			}

			// 注入 steering 消息
			for _, m := range pending {
				pm := m
				if err := emit(Event{Type: EventMessageStart, Message: &pm}); err != nil {
					return err
				}
				if err := emit(Event{Type: EventMessageEnd, Message: &pm}); err != nil {
					return err
				}
				curCtx.Messages = append(curCtx.Messages, m)
				*newMessages = append(*newMessages, m)
			}
			pending = nil

			// 流式获取 assistant 响应
			msg, err := streamAssistantResponse(ctx, curCtx, cfg, streamFn, emit)
			if err != nil {
				return err
			}
			*newMessages = append(*newMessages, msg)

			if msg.StopReason == llm.StopError || msg.StopReason == llm.StopAborted {
				m := msg
				if err := emit(Event{Type: EventTurnEnd, Message: &m}); err != nil {
					return err
				}
				return emit(Event{Type: EventAgentEnd, Messages: *newMessages})
			}

			toolCalls := msg.ToolCalls()
			var toolResults []AgentMessage
			hasMoreToolCalls = false
			if len(toolCalls) > 0 {
				var batch executedBatch
				if msg.StopReason == llm.StopLength {
					// 截断保护：参数可能被截断，整批判失败（pi 同款防御）
					batch, err = failTruncatedToolCalls(toolCalls, emit)
				} else {
					batch, err = executeToolCalls(ctx, curCtx, msg, toolCalls, cfg, emit)
				}
				if err != nil {
					return err
				}
				toolResults = batch.messages
				hasMoreToolCalls = !batch.terminate
				curCtx.Messages = append(curCtx.Messages, toolResults...)
				*newMessages = append(*newMessages, toolResults...)
			}

			m := msg
			if err := emit(Event{Type: EventTurnEnd, Message: &m, ToolResults: toolResults}); err != nil {
				return err
			}

			if cfg.prepareNext != nil {
				upd, err := cfg.prepareNext(ctx)
				if err != nil {
					return err
				}
				if upd != nil && upd.Model != nil {
					cfg.model = *upd.Model
				}
			}

			if cfg.shouldStop != nil {
				stop, err := cfg.shouldStop(ctx, ShouldStopAfterTurnContext{
					Message: msg, ToolResults: toolResults, NewMessages: *newMessages,
				})
				if err != nil {
					return err
				}
				if stop {
					return emit(Event{Type: EventAgentEnd, Messages: *newMessages})
				}
			}

			if cfg.getSteering != nil {
				pending = cfg.getSteering(ctx)
			}
		}

		// 本要停止，检查 follow-up 队列
		if cfg.getFollowUp != nil {
			if fu := cfg.getFollowUp(ctx); len(fu) > 0 {
				pending = fu
				continue
			}
		}
		break
	}

	return emit(Event{Type: EventAgentEnd, Messages: *newMessages})
}

// streamAssistantResponse 执行一次 LLM 流式调用并把流事件转成 message_* 事件。
// 这里是 AgentMessage[] → llm.Message[] 的转换边界。
func streamAssistantResponse(ctx context.Context, curCtx *Context, cfg loopConfig, streamFn llm.StreamFn, emit eventSink) (AgentMessage, error) {
	messages := curCtx.Messages
	var err error
	if cfg.transformCtx != nil {
		messages, err = cfg.transformCtx(ctx, messages)
		if err != nil {
			return AgentMessage{}, err
		}
	}
	convert := cfg.convertToLLM
	if convert == nil {
		convert = DefaultConvertToLLM
	}
	llmMessages, err := convert(messages)
	if err != nil {
		return AgentMessage{}, err
	}

	llmCtx := llm.Context{
		SystemPrompt: curCtx.SystemPrompt,
		Messages:     llmMessages,
		Tools:        toolSchemas(curCtx.Tools),
	}

	opts := cfg.streamOpts
	if cfg.getAPIKey != nil {
		opts.APIKey = cfg.getAPIKey(cfg.model.Provider)
	}

	stream := streamFn(ctx, cfg.model, llmCtx, opts)

	var partial *AgentMessage
	for ev := range stream.Events() {
		switch ev.Type {
		case llm.EventStart:
			pm := AgentMessage{Message: ev.Partial}
			partial = &pm
			curCtx.Messages = append(curCtx.Messages, pm)
			if err := emit(Event{Type: EventMessageStart, Message: &pm}); err != nil {
				return AgentMessage{}, err
			}
		case llm.EventTextStart, llm.EventTextDelta, llm.EventTextEnd,
			llm.EventThinkingStart, llm.EventThinkingDelta, llm.EventThinkingEnd,
			llm.EventToolCallStart, llm.EventToolCallDelta, llm.EventToolCallEnd:
			if partial != nil {
				pm := AgentMessage{Message: ev.Partial}
				partial = &pm
				curCtx.Messages[len(curCtx.Messages)-1] = pm
				evCopy := ev
				if err := emit(Event{Type: EventMessageUpdate, Message: &pm, StreamEvent: &evCopy}); err != nil {
					return AgentMessage{}, err
				}
			}
		case llm.EventDone, llm.EventError:
			final, ferr := stream.Result()
			fm := AgentMessage{Message: final}
			if partial != nil {
				curCtx.Messages[len(curCtx.Messages)-1] = fm
			} else {
				curCtx.Messages = append(curCtx.Messages, fm)
				if err := emit(Event{Type: EventMessageStart, Message: &fm}); err != nil {
					return AgentMessage{}, ferr
				}
			}
			if ferr != nil && final.StopReason == "" {
				fm.StopReason = llm.StopError
			}
			return fm, emit(Event{Type: EventMessageEnd, Message: &fm})
		}
	}
	// 流异常结束（未发 done/error）
	final, ferr := stream.Result()
	fm := AgentMessage{Message: final}
	if fm.StopReason == "" {
		fm.StopReason = llm.StopError
	}
	if ferr != nil {
		return fm, nil // 错误已通过 stopReason 传递，事件流保持一致
	}
	return fm, emit(Event{Type: EventMessageEnd, Message: &fm})
}

func toolSchemas(tools []AgentTool) []llm.Tool {
	var out []llm.Tool
	for _, t := range tools {
		out = append(out, t.Schema())
	}
	return out
}

// ---- 工具执行三段管线：prepare → execute → finalize ----

type executedBatch struct {
	messages  []AgentMessage
	terminate bool
}

// failTruncatedToolCalls 对应 pi 的 failToolCallsFromTruncatedMessage。
func failTruncatedToolCalls(toolCalls []*llm.ToolCallContent, emit eventSink) (executedBatch, error) {
	var batch executedBatch
	for _, tc := range toolCalls {
		if err := emit(Event{Type: EventToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments}); err != nil {
			return batch, err
		}
		result := ErrorToolResult(fmt.Sprintf(
			"Tool call %q was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.",
			tc.Name))
		if err := emit(Event{Type: EventToolExecutionEnd, ToolCallID: tc.ID, ToolName: tc.Name, Result: &result, IsError: true}); err != nil {
			return batch, err
		}
		tr := toolResultMessage(tc, result, true)
		if err := emitToolResult(tr, emit); err != nil {
			return batch, err
		}
		batch.messages = append(batch.messages, tr)
	}
	return batch, nil
}

func executeToolCalls(ctx context.Context, curCtx *Context, msg AgentMessage, toolCalls []*llm.ToolCallContent, cfg loopConfig, emit eventSink) (executedBatch, error) {
	sequential := cfg.toolExecution == ExecutionSequential
	if !sequential {
		for _, tc := range toolCalls {
			if t := findTool(curCtx.Tools, tc.Name); t != nil && t.ExecutionMode() == ExecutionSequential {
				sequential = true
				break
			}
		}
	}
	if sequential {
		return executeSequential(ctx, curCtx, msg, toolCalls, cfg, emit)
	}
	return executeParallel(ctx, curCtx, msg, toolCalls, cfg, emit)
}

func executeSequential(ctx context.Context, curCtx *Context, msg AgentMessage, toolCalls []*llm.ToolCallContent, cfg loopConfig, emit eventSink) (executedBatch, error) {
	var batch executedBatch
	var finals []finalizedCall
	for _, tc := range toolCalls {
		if err := emit(Event{Type: EventToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments}); err != nil {
			return batch, err
		}
		fin, err := runOneToolCall(ctx, curCtx, msg, tc, cfg, emit)
		if err != nil {
			return batch, err
		}
		finals = append(finals, fin)
		if err := emit(Event{Type: EventToolExecutionEnd, ToolCallID: tc.ID, ToolName: tc.Name, Result: &fin.result, IsError: fin.isError}); err != nil {
			return batch, err
		}
		tr := toolResultMessage(tc, fin.result, fin.isError)
		if err := emitToolResult(tr, emit); err != nil {
			return batch, err
		}
		batch.messages = append(batch.messages, tr)
		if ctx.Err() != nil {
			break
		}
	}
	batch.terminate = shouldTerminate(finals)
	return batch, nil
}

func executeParallel(ctx context.Context, curCtx *Context, msg AgentMessage, toolCalls []*llm.ToolCallContent, cfg loopConfig, emit eventSink) (executedBatch, error) {
	var batch executedBatch
	finals := make([]finalizedCall, len(toolCalls))
	var wg sync.WaitGroup
	for i, tc := range toolCalls {
		if err := emit(Event{Type: EventToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments}); err != nil {
			return batch, err
		}
		wg.Add(1)
		go func(i int, tc *llm.ToolCallContent) {
			defer wg.Done()
			fin, err := runOneToolCall(ctx, curCtx, msg, tc, cfg, emit)
			if err != nil {
				fin = finalizedCall{result: ErrorToolResult(err.Error()), isError: true}
			}
			finals[i] = fin
			// 注意：tool_execution_end 可能在并发下乱序到达（pi 亦如此）
			_ = emit(Event{Type: EventToolExecutionEnd, ToolCallID: tc.ID, ToolName: tc.Name, Result: &fin.result, IsError: fin.isError})
		}(i, tc)
	}
	wg.Wait()
	// toolResult 消息严格按原顺序入上下文
	for i, tc := range toolCalls {
		tr := toolResultMessage(tc, finals[i].result, finals[i].isError)
		if err := emitToolResult(tr, emit); err != nil {
			return batch, err
		}
		batch.messages = append(batch.messages, tr)
	}
	batch.terminate = shouldTerminate(finals)
	return batch, nil
}

type finalizedCall struct {
	result  ToolResult
	isError bool
}

// runOneToolCall 完成 prepare → execute → finalize 三段。
func runOneToolCall(ctx context.Context, curCtx *Context, msg AgentMessage, tc *llm.ToolCallContent, cfg loopConfig, emit eventSink) (finalizedCall, error) {
	// prepare：查工具 + 校验参数 + beforeToolCall 钩子
	tool := findTool(curCtx.Tools, tc.Name)
	if tool == nil {
		return finalizedCall{result: ErrorToolResult("Tool " + tc.Name + " not found"), isError: true}, nil
	}
	if err := llm.ValidateToolArguments(tool.Schema(), tc.Arguments); err != nil {
		return finalizedCall{result: ErrorToolResult(err.Error()), isError: true}, nil
	}
	if cfg.beforeToolCall != nil {
		before, err := cfg.beforeToolCall(ctx, BeforeToolCallContext{
			AssistantMessage: msg, ToolCall: *tc, Args: tc.Arguments,
		})
		if err != nil {
			return finalizedCall{result: ErrorToolResult(err.Error()), isError: true}, nil
		}
		if before != nil && before.Block {
			reason := before.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			res := ErrorToolResult(reason)
			res.Terminate = before.Terminate
			return finalizedCall{result: res, isError: true}, nil
		}
	}
	if ctx.Err() != nil {
		return finalizedCall{result: ErrorToolResult("Operation aborted"), isError: true}, nil
	}

	// execute：支持 onUpdate 流式 partial result
	onUpdate := func(pr ToolResult) {
		_ = emit(Event{
			Type: EventToolExecutionUpdate, ToolCallID: tc.ID, ToolName: tc.Name,
			Args: tc.Arguments, PartialResult: &pr,
		})
	}
	result, execErr := tool.Execute(ctx, tc.ID, tc.Arguments, onUpdate)
	if execErr != nil {
		result = ErrorToolResult(execErr.Error())
	}
	fin := finalizedCall{result: result, isError: execErr != nil}

	// finalize：afterToolCall 钩子可改写结果
	if cfg.afterToolCall != nil {
		after, err := cfg.afterToolCall(ctx, AfterToolCallContext{
			AssistantMessage: msg, ToolCall: *tc, Result: fin.result, IsError: fin.isError,
		})
		if err != nil {
			fin = finalizedCall{result: ErrorToolResult(err.Error()), isError: true}
		} else if after != nil {
			if after.Content != nil {
				fin.result.Content = *after.Content
			}
			if after.IsError != nil {
				fin.isError = *after.IsError
			}
			if after.Terminate != nil {
				fin.result.Terminate = *after.Terminate
			}
		}
	}
	return fin, nil
}

func shouldTerminate(finals []finalizedCall) bool {
	if len(finals) == 0 {
		return false
	}
	for _, f := range finals {
		if !f.result.Terminate {
			return false
		}
	}
	return true
}

func findTool(tools []AgentTool, name string) AgentTool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func toolResultMessage(tc *llm.ToolCallContent, result ToolResult, isErr bool) AgentMessage {
	content := result.Content
	if content == nil {
		content = []llm.Content{}
	}
	return AgentMessage{
		Message: llm.NewToolResultMessage(tc.ID, tc.Name, content, isErr),
		Payload: result.Details,
	}
}

func emitToolResult(tr AgentMessage, emit eventSink) error {
	m := tr
	if err := emit(Event{Type: EventMessageStart, Message: &m}); err != nil {
		return err
	}
	return emit(Event{Type: EventMessageEnd, Message: &m})
}

// DefaultConvertToLLM 默认转换：过滤自定义消息（对应 pi 的 defaultConvertToLlm）。
func DefaultConvertToLLM(messages []AgentMessage) ([]llm.Message, error) {
	var out []llm.Message
	for _, m := range messages {
		if m.Custom {
			continue
		}
		switch m.Role {
		case llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult:
			out = append(out, m.Message)
		}
	}
	return out, nil
}
