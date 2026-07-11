package gateway

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// ResumeStore is a bounded LRU mapping a conversation-prefix fingerprint to
// the CLI session id that produced that prefix. It is the whole memory of
// the resume feature: a miss simply means the request takes the flatten-and-
// replay path, so eviction is always safe.
type ResumeStore struct {
	mu      sync.Mutex
	max     int
	order   *list.List // front = most recently used; values are *resumeEntry
	entries map[string]*list.Element
}

type resumeEntry struct {
	key          string
	cliSessionID string
}

func NewResumeStore(max int) *ResumeStore {
	if max <= 0 {
		max = 32
	}
	return &ResumeStore{
		max:     max,
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

// Get returns the CLI session id stored under key and marks it most recently
// used. A nil store (resume disabled) always misses.
func (rs *ResumeStore) Get(key string) (string, bool) {
	if rs == nil || key == "" {
		return "", false
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	el, ok := rs.entries[key]
	if !ok {
		return "", false
	}
	rs.order.MoveToFront(el)
	return el.Value.(*resumeEntry).cliSessionID, true
}

// Put records the CLI session id for key, evicting the least recently used
// entry when the store is at its bound.
func (rs *ResumeStore) Put(key, cliSessionID string) {
	if rs == nil || key == "" || cliSessionID == "" {
		return
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if el, ok := rs.entries[key]; ok {
		el.Value.(*resumeEntry).cliSessionID = cliSessionID
		rs.order.MoveToFront(el)
		return
	}

	el := rs.order.PushFront(&resumeEntry{key: key, cliSessionID: cliSessionID})
	rs.entries[key] = el

	for rs.order.Len() > rs.max {
		oldest := rs.order.Back()
		if oldest == nil {
			break
		}
		rs.order.Remove(oldest)
		delete(rs.entries, oldest.Value.(*resumeEntry).key)
	}
}

func (rs *ResumeStore) Len() int {
	if rs == nil {
		return 0
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.order.Len()
}

// resumeFingerprint hashes a conversation prefix together with the execution
// parameters that shaped it. Model and effort are part of the key because a
// CLI session carries them: resuming a sonnet session for an opus request
// would silently answer with the wrong model.
func resumeFingerprint(messages []OpenAIMessage, model, effort string) string {
	type hashedTurn struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	turns := make([]hashedTurn, 0, len(messages))
	for _, msg := range messages {
		text, err := textOf(msg)
		if err != nil {
			return ""
		}
		turns = append(turns, hashedTurn{Role: msg.Role, Content: text})
	}

	payload, err := json.Marshal(struct {
		Model  string       `json:"model"`
		Effort string       `json:"effort"`
		Turns  []hashedTurn `json:"turns"`
	}{model, effort, turns})
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// isResumableConversation reports whether a request's history can be carried
// by a CLI session. Tool history cannot: the tool loop has its own live
// session mechanism (ADR 0001), and a cold tool transcript has no CLI session
// to resume into.
func isResumableConversation(messages []OpenAIMessage) bool {
	for _, msg := range messages {
		if msg.Role == "tool" || len(msg.ToolCalls) > 0 {
			return false
		}
		if _, err := textOf(msg); err != nil {
			return false
		}
	}
	return true
}

// splitResumeTurn divides a conversation into the prefix a stored CLI session
// would already hold (everything through the last assistant reply) and the
// new user text to send into that session. ok is false when there is nothing
// to resume: a first turn, or a shape the CLI session cannot represent.
func splitResumeTurn(messages []OpenAIMessage) (prefix []OpenAIMessage, userText string, ok bool) {
	lastAssistant := -1
	for i, msg := range messages {
		if msg.Role == "assistant" {
			lastAssistant = i
		}
	}
	if lastAssistant < 0 {
		return nil, "", false
	}

	tail := messages[lastAssistant+1:]
	if len(tail) == 0 {
		return nil, "", false
	}

	var userParts []string
	for _, msg := range tail {
		if msg.Role != "user" {
			return nil, "", false
		}
		text, err := textOf(msg)
		if err != nil {
			return nil, "", false
		}
		userParts = append(userParts, text)
	}

	userText = joinNonEmpty(userParts, "\n\n")
	if userText == "" {
		return nil, "", false
	}

	return messages[:lastAssistant+1], userText, true
}

func joinNonEmpty(parts []string, sep string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += p
	}
	return out
}

// systemPromptOf collects the system/developer messages the same way
// BuildPrompt does, without the transcript instructions: a resumed session
// already holds the real turn structure, so it needs no transcript framing.
func systemPromptOf(messages []OpenAIMessage) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role != "system" && msg.Role != "developer" {
			continue
		}
		text, err := textOf(msg)
		if err != nil {
			return defaultSystemPrompt
		}
		parts = append(parts, text)
	}

	prompt := joinNonEmpty(parts, "\n\n")
	if prompt == "" {
		return defaultSystemPrompt
	}
	return prompt
}
