package llm

import (
	"context"
	"fmt"
	"sync"
)

// Provider 是一个模型供应商（对应 pi 的 provider 工厂产物）。
type Provider interface {
	ID() string
	Models() []Model
	Stream(ctx context.Context, model Model, c Context, opts StreamOptions) *EventStream
}

// Registry 是 provider/模型注册表（对应 pi 的 createModels()）。
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// SetProvider 注册或替换一个 provider。
func (r *Registry) SetProvider(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.ID()] = p
}

// GetModel 按 "provider/model-id" 或分离参数查找模型。
func (r *Registry) GetModel(providerID, modelID string) (Model, error) {
	r.mu.RLock()
	p, ok := r.providers[providerID]
	r.mu.RUnlock()
	if !ok {
		return Model{}, fmt.Errorf("provider %q not registered", providerID)
	}
	for _, m := range p.Models() {
		if m.ID == modelID {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("model %q not found in provider %q", modelID, providerID)
}

// StreamSimple 用注册表解析模型所在 provider 并发起流式调用，
// 可直接作为 agent 的 StreamFn（对应 pi 的 models.streamSimple）。
func (r *Registry) StreamSimple(ctx context.Context, model Model, c Context, opts StreamOptions) *EventStream {
	r.mu.RLock()
	p, ok := r.providers[model.Provider]
	r.mu.RUnlock()
	if !ok {
		s := NewEventStream()
		errMsg := Message{Role: RoleAssistant, StopReason: StopError, Timestamp: nowMs()}
		s.Finish(errMsg, fmt.Errorf("provider %q not registered", model.Provider))
		return s
	}
	return p.Stream(ctx, model, c, opts)
}

// ListModels 列出全部已注册模型。
func (r *Registry) ListModels() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Model
	for _, p := range r.providers {
		out = append(out, p.Models()...)
	}
	return out
}
