package llm

import (
	"sync"
	"time"
)

func nowMs() int64 { return time.Now().UnixMilli() }

// EventType 是流式助手事件类型（对应 pi 的 AssistantMessageEvent）。
type EventType string

const (
	EventStart         EventType = "start"
	EventTextStart     EventType = "text_start"
	EventTextDelta     EventType = "text_delta"
	EventTextEnd       EventType = "text_end"
	EventThinkingStart EventType = "thinking_start"
	EventThinkingDelta EventType = "thinking_delta"
	EventThinkingEnd   EventType = "thinking_end"
	EventToolCallStart EventType = "toolcall_start"
	EventToolCallDelta EventType = "toolcall_delta"
	EventToolCallEnd   EventType = "toolcall_end"
	EventDone          EventType = "done"
	EventError         EventType = "error"
)

// Event 携带累积中的 partial message 快照——与 pi 一致，
// 消费者拿到 Partial 即可直接渲染，无需自己合并 delta。
type Event struct {
	Type    EventType
	Delta   string  // 仅 *_delta 事件有效
	Partial Message // 当前累积的 assistant 消息快照
	Err     error   // 仅 error 事件有效
}

// EventStream 是 channel 化的事件流（对应 pi 的 EventStream）。
// Provider 在 goroutine 里推送事件，Close 后 Result() 返回最终消息。
type EventStream struct {
	ch     chan Event
	mu     sync.Mutex
	result Message
	err    error
	done   chan struct{}
}

// NewEventStream 创建一个事件流。
func NewEventStream() *EventStream {
	return &EventStream{ch: make(chan Event, 64), done: make(chan struct{})}
}

// Push 推送一个事件（provider 侧调用）。
func (s *EventStream) Push(ev Event) {
	select {
	case s.ch <- ev:
	case <-s.done:
	}
}

// Finish 设置最终结果并关闭流。
func (s *EventStream) Finish(result Message, err error) {
	s.mu.Lock()
	s.result, s.err = result, err
	s.mu.Unlock()
	close(s.done)
	close(s.ch)
}

// Events 返回事件通道（消费者 range 读到关闭为止）。
func (s *EventStream) Events() <-chan Event { return s.ch }

// Result 在流结束后返回最终 assistant 消息（对应 pi 的 response.result()）。
func (s *EventStream) Result() (Message, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}
