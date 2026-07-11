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

// ToolUseBlock is one tool invocation announced by the model in the
// CLI's stream-json output.
type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// SessionEvent is one parsed occurrence on a session's CLI stream. The
// reader goroutine produces these; the HTTP handler driving the current
// turn consumes them.
type SessionEvent struct {
	Type       string // "text" | "tool_calls" | "stop" | "run_end"
	Text       string
	Calls      []*PendingToolCall
	StopReason *string
	Model      string
	Usage      *Usage
	CostUSD    *float64
	ResultText *string
	Err        error
}

// PendingToolCall is a tool call announced on the stream and not yet
// resolved by a client-supplied result. Resolver is buffered so a result
// arriving before the CLI's MCP tools/call request is never lost.
type PendingToolCall struct {
	ID        string
	Name      string
	Arguments string // canonical JSON
	Resolver  chan string
	Claimed   bool
	Resolved  bool
}

type Session struct {
	ID           string
	Cmd          *exec.Cmd
	Tools        []ToolSchema
	Events       chan SessionEvent
	Pending      []*PendingToolCall // announcement order
	PendingByID  map[string]*PendingToolCall
	CreatedAt    time.Time
	LastActivity time.Time
	Reaped       bool
	Mu           sync.Mutex
}

type SessionManager struct {
	sessions            map[string]*Session
	toolCallIDToSession map[string]string
	mu                  sync.RWMutex
	maxSessions         int
	toolTimeoutMS       int
	sessionIdleMS       int
	done                chan struct{}
}

func NewSessionManager(maxSessions, toolTimeoutMS, sessionIdleMS int) *SessionManager {
	sm := &SessionManager{
		sessions:            make(map[string]*Session),
		toolCallIDToSession: make(map[string]string),
		maxSessions:         maxSessions,
		toolTimeoutMS:       toolTimeoutMS,
		sessionIdleMS:       sessionIdleMS,
		done:                make(chan struct{}),
	}
	go sm.sweepLoop()
	return sm
}

func (sm *SessionManager) sweepLoop() {
	interval := sm.toolTimeoutMS
	if sm.sessionIdleMS < interval {
		interval = sm.sessionIdleMS
	}
	interval = interval / 10
	if interval < 10 {
		interval = 10
	}
	if interval > 5000 {
		interval = 5000
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sm.sweep()
		case <-sm.done:
			return
		}
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
		hasPending := false
		for _, p := range sess.Pending {
			if !p.Resolved {
				hasPending = true
				break
			}
		}
		age := now.Sub(sess.LastActivity)
		sess.Mu.Unlock()

		if (hasPending && age > toolTimeoutDur) || (!hasPending && age > idleTimeoutDur) {
			sm.reapSessionLocked(id)
		}
	}
}

// CreateSession registers a new session, or returns nil when the
// MaxSessions cap is reached.
func (sm *SessionManager) CreateSession(tools []ToolSchema) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if len(sm.sessions) >= sm.maxSessions {
		return nil
	}

	sess := &Session{
		ID:           newSessionID(),
		Tools:        tools,
		Events:       make(chan SessionEvent, 64),
		PendingByID:  make(map[string]*PendingToolCall),
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	}
	sm.sessions[sess.ID] = sess
	return sess
}

func (sm *SessionManager) GetSession(sessionID string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[sessionID]
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

// RegisterAnnouncedCalls mints tool_call_ids for tool_use blocks announced
// on the stream, records them as pending on the session, and indexes them
// globally for continuation correlation. Called from the stream reader at
// announcement time so an MCP tools/call can never race ahead of
// registration.
func (sm *SessionManager) RegisterAnnouncedCalls(sess *Session, uses []ToolUseBlock) []*PendingToolCall {
	calls := make([]*PendingToolCall, 0, len(uses))

	sess.Mu.Lock()
	if sess.Reaped {
		sess.Mu.Unlock()
		return nil
	}
	for _, u := range uses {
		p := &PendingToolCall{
			ID:        NewToolCallID(),
			Name:      u.Name,
			Arguments: canonicalJSON(u.Input),
			Resolver:  make(chan string, 1),
		}
		sess.Pending = append(sess.Pending, p)
		sess.PendingByID[p.ID] = p
		calls = append(calls, p)
	}
	sess.LastActivity = time.Now()
	sess.Mu.Unlock()

	sm.mu.Lock()
	for _, p := range calls {
		sm.toolCallIDToSession[p.ID] = sess.ID
	}
	sm.mu.Unlock()

	return calls
}

// ResolveToolCall delivers a client-supplied result to a pending call.
// Returns false when the tool_call_id is unknown or already resolved.
func (sm *SessionManager) ResolveToolCall(sess *Session, toolCallID, result string) bool {
	sess.Mu.Lock()
	p := sess.PendingByID[toolCallID]
	if p == nil || p.Resolved {
		sess.Mu.Unlock()
		return false
	}
	p.Resolved = true
	p.Resolver <- result
	sess.LastActivity = time.Now()
	sess.Mu.Unlock()

	sm.mu.Lock()
	delete(sm.toolCallIDToSession, toolCallID)
	sm.mu.Unlock()
	return true
}

// ClaimPendingCall matches an MCP tools/call to an announced-but-unclaimed
// pending call by (name, canonical arguments) in announcement order.
func (sess *Session) ClaimPendingCall(name, canonArgs string) *PendingToolCall {
	sess.Mu.Lock()
	defer sess.Mu.Unlock()

	for _, p := range sess.Pending {
		if !p.Claimed && p.Name == name && p.Arguments == canonArgs {
			p.Claimed = true
			sess.LastActivity = time.Now()
			return p
		}
	}
	return nil
}

// ReapSuperseded reaps every live session that the incoming request's
// history names by tool_call_id but does not resume — keep is the session
// this request continues, if any. A client that abandons a tool loop (the
// end user interrupts and sends a new turn instead of the tool results)
// otherwise strands the parked session: it holds a MaxSessions slot and a
// live CLI process until the idle sweeper fires, minutes later. Only
// unresolved calls are indexed in toolCallIDToSession, so a hit here means
// the session is genuinely still waiting on a result that is not coming.
// Returns the number of sessions reaped.
func (sm *SessionManager) ReapSuperseded(toolCallIDs []string, keep *Session) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	superseded := make(map[string]struct{})
	for _, toolCallID := range toolCallIDs {
		sessionID, ok := sm.toolCallIDToSession[toolCallID]
		if !ok {
			continue
		}
		if keep != nil && sessionID == keep.ID {
			continue
		}
		superseded[sessionID] = struct{}{}
	}

	for sessionID := range superseded {
		sm.reapSessionLocked(sessionID)
	}
	return len(superseded)
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
	sess.Reaped = true
	for _, p := range sess.Pending {
		if !p.Resolved {
			p.Resolved = true
			close(p.Resolver)
		}
	}
	if sess.Cmd != nil && sess.Cmd.Process != nil {
		_ = sess.Cmd.Process.Kill()
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

func (sm *SessionManager) Close() {
	sm.mu.Lock()
	ids := make([]string, 0, len(sm.sessions))
	for id := range sm.sessions {
		ids = append(ids, id)
	}
	for _, id := range ids {
		sm.reapSessionLocked(id)
	}
	sm.mu.Unlock()

	select {
	case <-sm.done:
	default:
		close(sm.done)
	}
}

// canonicalJSON re-serializes a JSON value so that two byte-different but
// semantically equal argument objects compare equal (Go marshals map keys
// sorted). Empty input canonicalizes to "{}".
func canonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
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
