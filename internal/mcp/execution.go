package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/google/uuid"
)

type OutputChunk struct {
	Stream string `json:"stream"` // "stdout" or "stderr"
	Data   string `json:"chunk"`
}

type Execution struct {
	ID        string
	SessionID string
	SandboxID uuid.UUID
	Command   string
	Status    string // "running", "completed", "failed", "cancelled"
	StartedAt time.Time
	DoneAt    time.Time
	cancel    context.CancelFunc

	mu         sync.Mutex
	outputBuf  []OutputChunk
	lastPolled int
	ExitCode   *int
	Error      string
}

func (e *Execution) AppendOutput(stream, data string) {
	e.mu.Lock()
	e.outputBuf = append(e.outputBuf, OutputChunk{Stream: stream, Data: data})
	e.mu.Unlock()
}

func (e *Execution) SetDone(exitCode int) {
	e.mu.Lock()
	e.ExitCode = &exitCode
	e.mu.Unlock()
}

func (e *Execution) SetError(msg string) {
	e.mu.Lock()
	e.Error = msg
	e.mu.Unlock()
}

// PollOutput returns output chunks since the last poll and advances the cursor.
func (e *Execution) PollOutput() (chunks []OutputChunk, status string, exitCode *int, errMsg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastPolled < len(e.outputBuf) {
		chunks = make([]OutputChunk, len(e.outputBuf)-e.lastPolled)
		copy(chunks, e.outputBuf[e.lastPolled:])
		e.lastPolled = len(e.outputBuf)
	}
	return chunks, e.Status, e.ExitCode, e.Error
}

type ExecutionRegistry struct {
	mu    sync.RWMutex
	execs map[string]*Execution
}

func NewExecutionRegistry() *ExecutionRegistry {
	return &ExecutionRegistry{
		execs: make(map[string]*Execution),
	}
}

func (r *ExecutionRegistry) Create(sessionID string, sandboxID uuid.UUID, command string, cancel context.CancelFunc) *Execution {
	id := newExecID()
	e := &Execution{
		ID:        id,
		SessionID: sessionID,
		SandboxID: sandboxID,
		Command:   command,
		Status:    "running",
		StartedAt: time.Now(),
		cancel:    cancel,
	}
	r.mu.Lock()
	r.execs[id] = e
	r.mu.Unlock()
	return e
}

func (r *ExecutionRegistry) Get(id string) *Execution {
	r.mu.RLock()
	e := r.execs[id]
	r.mu.RUnlock()
	return e
}

func (r *ExecutionRegistry) Complete(id, status string) {
	r.mu.Lock()
	if e, ok := r.execs[id]; ok {
		e.Status = status
		e.DoneAt = time.Now()
	}
	r.mu.Unlock()
}

func (r *ExecutionRegistry) Cancel(id string) bool {
	r.mu.RLock()
	e := r.execs[id]
	r.mu.RUnlock()
	if e == nil || e.Status != "running" {
		return false
	}
	if e.cancel != nil {
		e.cancel()
	}
	r.mu.Lock()
	e.Status = "cancelled"
	e.DoneAt = time.Now()
	r.mu.Unlock()
	return true
}

func (r *ExecutionRegistry) CountForSession(sessionID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.execs {
		if e.SessionID == sessionID && e.Status == "running" {
			n++
		}
	}
	return n
}

func (r *ExecutionRegistry) CleanupDone(olderThan time.Duration) {
	cutoff := time.Now().Add(-olderThan)
	r.mu.Lock()
	for id, e := range r.execs {
		if e.Status != "running" && e.DoneAt.Before(cutoff) {
			delete(r.execs, id)
		}
	}
	r.mu.Unlock()
}

func newExecID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "exec_" + hex.EncodeToString(b[:])
}
