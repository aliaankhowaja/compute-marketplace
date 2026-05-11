package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type PodState string

const (
	StateRunning    PodState = "running"
	StateStopped    PodState = "stopped"
	StateTerminated PodState = "terminated"
)

type Pod struct {
	ID        string
	Name      string
	Env       map[string]string
	State     PodState
	CreatedAt time.Time
}

type Store struct {
	mu   sync.RWMutex
	pods map[string]*Pod
}

func NewStore() *Store {
	return &Store{pods: make(map[string]*Pod)}
}

func (s *Store) Create(name string, env map[string]string) *Pod {
	id := randomID()
	p := &Pod{
		ID:        id,
		Name:      name,
		Env:       env,
		State:     StateRunning,
		CreatedAt: time.Now(),
	}
	s.mu.Lock()
	s.pods[id] = p
	s.mu.Unlock()
	return p
}

func (s *Store) Get(id string) (*Pod, bool) {
	s.mu.RLock()
	p, ok := s.pods[id]
	s.mu.RUnlock()
	return p, ok
}

func (s *Store) Stop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pods[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if p.State == StateTerminated {
		return fmt.Errorf("pod terminated")
	}
	p.State = StateStopped
	return nil
}

func (s *Store) Start(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pods[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if p.State == StateTerminated {
		return fmt.Errorf("pod terminated")
	}
	p.State = StateRunning
	return nil
}

func (s *Store) UpdateEnv(id string, env map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pods[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	if p.State == StateTerminated {
		return fmt.Errorf("pod terminated")
	}
	if p.Env == nil {
		p.Env = make(map[string]string)
	}
	for k, v := range env {
		p.Env[k] = v
	}
	return nil
}

func (s *Store) Terminate(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pods[id]
	if !ok {
		return nil
	}
	p.State = StateTerminated
	return nil
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
