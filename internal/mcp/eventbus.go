package mcp

import (
	"sync"

	"github.com/google/uuid"
)

// SandboxEvent is published to SSE subscribers when billing state changes.
type SandboxEvent struct {
	Type      string         `json:"type"`
	SandboxID uuid.UUID      `json:"sandbox_id"`
	Data      map[string]any `json:"data,omitempty"`
}

// SandboxEventBus provides per-sandbox pub/sub for billing notifications.
type SandboxEventBus struct {
	mu       sync.RWMutex
	subs     map[uuid.UUID]map[chan SandboxEvent]struct{}
	sessions *SessionRegistry
}

func NewSandboxEventBus() *SandboxEventBus {
	return &SandboxEventBus{
		subs: make(map[uuid.UUID]map[chan SandboxEvent]struct{}),
	}
}

func (b *SandboxEventBus) Subscribe(sandboxID uuid.UUID, buf int) chan SandboxEvent {
	if buf <= 0 {
		buf = 16
	}
	ch := make(chan SandboxEvent, buf)
	b.mu.Lock()
	if b.subs[sandboxID] == nil {
		b.subs[sandboxID] = make(map[chan SandboxEvent]struct{})
	}
	b.subs[sandboxID][ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *SandboxEventBus) Unsubscribe(sandboxID uuid.UUID, ch chan SandboxEvent) {
	b.mu.Lock()
	if m := b.subs[sandboxID]; m != nil {
		delete(m, ch)
		if len(m) == 0 {
			delete(b.subs, sandboxID)
		}
	}
	b.mu.Unlock()
	close(ch)
}

func (b *SandboxEventBus) SetSessionRegistry(reg *SessionRegistry) {
	b.mu.Lock()
	b.sessions = reg
	b.mu.Unlock()
}

func (b *SandboxEventBus) Publish(ev SandboxEvent) {
	b.mu.RLock()
	for ch := range b.subs[ev.SandboxID] {
		select {
		case ch <- ev:
		default:
		}
	}
	reg := b.sessions
	b.mu.RUnlock()

	if reg != nil {
		reg.BridgeSandboxEvent(ev)
	}
}
