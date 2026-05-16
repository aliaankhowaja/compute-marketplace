package mcp

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

type SessionEvent struct {
	SeqID     int64            `json:"seq_id"`
	Type      string           `json:"type"`
	ExecID    string           `json:"execution_id,omitempty"`
	SandboxID uuid.UUID        `json:"sandbox_id,omitempty"`
	Data      map[string]any   `json:"data,omitempty"`
	RawJSON   json.RawMessage  `json:"raw_json,omitempty"`
	Timestamp time.Time        `json:"timestamp"`
}

type Session struct {
	ID        string
	Principal *auth.Principal

	mu         sync.Mutex
	sandboxes  map[uuid.UUID]struct{}
	eventCh    chan SessionEvent
	ringBuf    []SessionEvent
	ringCap    int
	nextSeq    int64
	createdAt  time.Time
	lastActive time.Time
}

func newSession(id string, principal *auth.Principal, eventBuf, ringCap int) *Session {
	if eventBuf <= 0 {
		eventBuf = 64
	}
	if ringCap <= 0 {
		ringCap = 256
	}
	now := time.Now()
	return &Session{
		ID:         id,
		Principal:  principal,
		sandboxes:  make(map[uuid.UUID]struct{}),
		eventCh:    make(chan SessionEvent, eventBuf),
		ringBuf:    make([]SessionEvent, 0, ringCap),
		ringCap:    ringCap,
		createdAt:  now,
		lastActive: now,
	}
}

func (s *Session) Emit(ev SessionEvent) {
	s.mu.Lock()
	s.nextSeq++
	ev.SeqID = s.nextSeq
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if len(s.ringBuf) >= s.ringCap {
		s.ringBuf = s.ringBuf[1:]
	}
	s.ringBuf = append(s.ringBuf, ev)
	s.lastActive = time.Now()
	s.mu.Unlock()

	select {
	case s.eventCh <- ev:
	default:
	}
}

func (s *Session) EventsSince(seqID int64) []SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SessionEvent
	for _, ev := range s.ringBuf {
		if ev.SeqID > seqID {
			out = append(out, ev)
		}
	}
	return out
}

func (s *Session) EventCh() <-chan SessionEvent {
	return s.eventCh
}

func (s *Session) TrackSandbox(id uuid.UUID) {
	s.mu.Lock()
	s.sandboxes[id] = struct{}{}
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *Session) HasSandbox(id uuid.UUID) bool {
	s.mu.Lock()
	_, ok := s.sandboxes[id]
	s.mu.Unlock()
	return ok
}

type SessionRegistry struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	bySandbox map[uuid.UUID]map[string]struct{}
	ttl       time.Duration
	ringCap   int
	closed    atomic.Bool
}

func NewSessionRegistry(ttl time.Duration, ringCap int) *SessionRegistry {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if ringCap <= 0 {
		ringCap = 256
	}
	return &SessionRegistry{
		sessions:  make(map[string]*Session),
		bySandbox: make(map[uuid.UUID]map[string]struct{}),
		ttl:       ttl,
		ringCap:   ringCap,
	}
}

func (r *SessionRegistry) Create(id string, principal *auth.Principal) *Session {
	s := newSession(id, principal, 64, r.ringCap)
	r.mu.Lock()
	r.sessions[id] = s
	r.mu.Unlock()
	return s
}

func (r *SessionRegistry) Get(id string) *Session {
	if id == "" {
		return nil
	}
	r.mu.RLock()
	s := r.sessions[id]
	r.mu.RUnlock()
	return s
}

func (r *SessionRegistry) TrackSandbox(sessionID string, sandboxID uuid.UUID) {
	r.mu.Lock()
	if s, ok := r.sessions[sessionID]; ok {
		s.TrackSandbox(sandboxID)
		if r.bySandbox[sandboxID] == nil {
			r.bySandbox[sandboxID] = make(map[string]struct{})
		}
		r.bySandbox[sandboxID][sessionID] = struct{}{}
	}
	r.mu.Unlock()
}

func (r *SessionRegistry) DeliverToSandbox(sandboxID uuid.UUID, ev SessionEvent) {
	r.mu.RLock()
	sids := r.bySandbox[sandboxID]
	ids := make([]string, 0, len(sids))
	for sid := range sids {
		ids = append(ids, sid)
	}
	r.mu.RUnlock()

	for _, sid := range ids {
		r.mu.RLock()
		s := r.sessions[sid]
		r.mu.RUnlock()
		if s != nil {
			s.Emit(ev)
		}
	}
}

func (r *SessionRegistry) BridgeSandboxEvent(ev SandboxEvent) {
	r.DeliverToSandbox(ev.SandboxID, SessionEvent{
		Type:      ev.Type,
		SandboxID: ev.SandboxID,
		Data:      ev.Data,
	})
}

func (r *SessionRegistry) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-r.ttl)
	for id, s := range r.sessions {
		s.mu.Lock()
		expired := s.lastActive.Before(cutoff)
		s.mu.Unlock()
		if expired {
			for sbID := range s.sandboxes {
				if m := r.bySandbox[sbID]; m != nil {
					delete(m, id)
					if len(m) == 0 {
						delete(r.bySandbox, sbID)
					}
				}
			}
			close(s.eventCh)
			delete(r.sessions, id)
		}
	}
}

func (r *SessionRegistry) StartCleanupLoop(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if r.closed.Load() {
				return
			}
			r.Cleanup()
		}
	}()
}

func (r *SessionRegistry) Close() {
	r.closed.Store(true)
}
