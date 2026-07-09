package gateway

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

type Session struct {
	ID              string
	Process         *exec.Cmd
	ProcessStarted  bool
	Tools           []ToolSchema
	PendingCalls    map[string]*PendingToolCall
	BufferedResults map[string]string
	CreatedAt       time.Time
	LastActivity    time.Time
	Mu              sync.Mutex
}

type PendingToolCall struct {
	Name      string
	Arguments json.RawMessage
	Resolver  chan string
}

type SessionManager struct {
	sessions          map[string]*Session
	toolCallIDToSession map[string]string
	mu                sync.RWMutex
	maxSessions       int
	toolTimeoutMS     int
	sessionIdleMS     int
	sweeper           chan struct{}
}

func NewSessionManager(maxSessions, toolTimeoutMS, sessionIdleMS int) *SessionManager {
	sm := &SessionManager{
		sessions:          make(map[string]*Session),
		toolCallIDToSession: make(map[string]string),
		maxSessions:       maxSessions,
		toolTimeoutMS:     toolTimeoutMS,
		sessionIdleMS:     sessionIdleMS,
		sweeper:           make(chan struct{}),
	}
	go sm.sweepLoop()
	return sm
}

func (sm *SessionManager) sweepLoop() {
	ticker := time.NewTicker(time.Duration(sm.toolTimeoutMS/10) * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		sm.sweep()
	}
}

func (sm *SessionManager) sweep() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	toolTimeoutDur := time.Duration(sm.toolTimeoutMS) * time.Millisecond
	idleTimeoutDur := time.Duration(sm.sessionIdleMS) * time.Millisecond

	for id, sess := range sm.sessions {
		sess.Mu.Lock()
		hasPending := len(sess.PendingCalls) > 0
		idle := now.Sub(sess.LastActivity) > idleTimeoutDur
		toolTimeout := hasPending && now.Sub(sess.LastActivity) > toolTimeoutDur
		sess.Mu.Unlock()

		if toolTimeout || idle {
			sm.reapSessionLocked(id)
		}
	}
}

func (sm *SessionManager) CreateSession(tools []ToolSchema) string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	id := newSessionID()
	sess := &Session{
		ID:              id,
		Tools:           tools,
		PendingCalls:    make(map[string]*PendingToolCall),
		BufferedResults: make(map[string]string),
		CreatedAt:       time.Now(),
		LastActivity:    time.Now(),
	}
	sm.sessions[id] = sess
	return id
}

func (sm *SessionManager) GetSession(sessionID string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[sessionID]
}

func (sm *SessionManager) RegisterToolCallID(toolCallID, sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.toolCallIDToSession[toolCallID] = sessionID
}

func (sm *SessionManager) GetSessionByToolCallID(toolCallID string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessionID, ok := sm.toolCallIDToSession[toolCallID]
	if !ok {
		return nil
	}
	return sm.sessions[sessionID]
}

func (sm *SessionManager) ReapSession(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.reapSessionLocked(sessionID)
}

func (sm *SessionManager) reapSessionLocked(sessionID string) {
	sess, ok := sm.sessions[sessionID]
	if !ok {
		return
	}

	sess.Mu.Lock()
	if sess.Process != nil && sess.ProcessStarted {
		sess.Process.Process.Kill()
	}
	sess.Mu.Unlock()

	delete(sm.sessions, sessionID)

	for toolCallID, sID := range sm.toolCallIDToSession {
		if sID == sessionID {
			delete(sm.toolCallIDToSession, toolCallID)
		}
	}
}

func (sm *SessionManager) GetSessionCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

func (sm *SessionManager) CanCreateSession() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions) < sm.maxSessions
}

func (sm *SessionManager) Close() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, sess := range sm.sessions {
		sess.Mu.Lock()
		if sess.Process != nil && sess.ProcessStarted {
			sess.Process.Process.Kill()
		}
		sess.Mu.Unlock()
	}
	sm.sessions = make(map[string]*Session)
	sm.toolCallIDToSession = make(map[string]string)
}

func newSessionID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%032x", b)
}

func NewToolCallID() string {
	b := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("call_%024x", b)
}
