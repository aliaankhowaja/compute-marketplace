package mcp

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

type Session struct {
	ID        string
	Principal *auth.Principal

	mu         sync.Mutex
	sandboxes  map[uuid.UUID]struct{}
	createdAt  time.Time
	lastActive time.Time
}

func newSession(id string, principal *auth.Principal) *Session {
	now := time.Now()
	return &Session{
		ID:         id,
		Principal:  principal,
		sandboxes:  make(map[uuid.UUID]struct{}),
		createdAt:  now,
		lastActive: now,
	}
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
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration
	closed   atomic.Bool
}

func NewSessionRegistry(ttl time.Duration) *SessionRegistry {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &SessionRegistry{
		sessions: make(map[string]*Session),
		ttl:      ttl,
	}
}

func (r *SessionRegistry) Create(id string, principal *auth.Principal) *Session {
	s := newSession(id, principal)
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
	}
	r.mu.Unlock()
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
