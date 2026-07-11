package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- stub CLI ----------------------------------------------------------

// stubRun is one invocation of the stub CLI, appended to the file named by
// CLAUDE_STUB_RECORD so a test can assert on the argv and the prompt the
// gateway actually handed the CLI.
type stubRun struct {
	Args      []string `json:"args"`
	Stdin     string   `json:"stdin"`
	SessionID string   `json:"session_id"`
}

// runResumeStub impersonates a session-persisting claude CLI: it echoes back
// the session id it was resumed with, or mints a fresh one, and reports it on
// the stream the way the real CLI does.
func runResumeStub() int {
	stdin, _ := io.ReadAll(os.Stdin)

	args := os.Args[1:]
	resumeID := ""
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) {
			resumeID = args[i+1]
		}
	}

	recordPath := os.Getenv("CLAUDE_STUB_RECORD")
	prior, _ := readStubRuns(recordPath)

	sessionID := resumeID
	if sessionID == "" {
		sessionID = fmt.Sprintf("cli-sess-%d", len(prior)+1)
	}

	if recordPath != "" {
		line, _ := json.Marshal(stubRun{Args: args, Stdin: string(stdin), SessionID: sessionID})
		f, err := os.OpenFile(recordPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 1
		}
		_, writeErr := fmt.Fprintf(f, "%s\n", line)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return 1
		}
	}

	answer := "reply from " + sessionID

	emit := func(v interface{}) {
		line, _ := json.Marshal(v)
		fmt.Println(string(line))
	}
	emit(map[string]interface{}{"type": "system", "subtype": "init", "session_id": sessionID})
	emit(map[string]interface{}{
		"type":       "stream_event",
		"session_id": sessionID,
		"event": map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": answer},
		},
	})
	emit(map[string]interface{}{
		"type":           "result",
		"session_id":     sessionID,
		"result":         answer,
		"stop_reason":    "end_turn",
		"total_cost_usd": 0.001,
		"usage": map[string]int{
			"input_tokens":                10,
			"output_tokens":               4,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		},
	})
	return 0
}

// --- test-side helpers -------------------------------------------------

func readStubRuns(path string) ([]stubRun, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var runs []stubRun
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var run stubRun
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// newResumeServer wires the argv-recording stub CLI to a gateway and returns
// the base URL plus an accessor for the runs the gateway spawned.
func newResumeServer(t *testing.T, cfg Config) (string, func() []stubRun) {
	t.Helper()

	recordPath := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("CLAUDE_STUB_MODE", "resume")
	t.Setenv("CLAUDE_STUB_RECORD", recordPath)

	_, ts := newRelayServer(t, cfg)

	return ts.URL, func() []stubRun {
		runs, err := readStubRuns(recordPath)
		if err != nil {
			t.Fatalf("read stub runs: %v", err)
		}
		return runs
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// chat posts one turn and returns the assistant reply text.
func chat(t *testing.T, baseURL string, messages []map[string]interface{}) string {
	t.Helper()

	status, resp := postChat(t, baseURL, map[string]interface{}{
		"model":    "sonnet",
		"messages": messages,
	})
	if status != 200 {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices in response")
	}
	text, _ := resp.Choices[0].Message.Content.(string)
	if text == "" {
		t.Fatalf("empty assistant reply")
	}
	return text
}

func userMsg(text string) map[string]interface{} {
	return map[string]interface{}{"role": "user", "content": text}
}

func assistantMsg(text string) map[string]interface{} {
	return map[string]interface{}{"role": "assistant", "content": text}
}

// --- tests -------------------------------------------------------------

// TestResumeSessionReusesCLISession proves the point of the feature: a second
// turn whose prefix matches the first resumes the same CLI session and sends
// only the new user message, instead of replaying a flattened transcript.
func TestResumeSessionReusesCLISession(t *testing.T) {
	baseURL, runs := newResumeServer(t, Config{ResumeSessions: true})

	first := chat(t, baseURL, []map[string]interface{}{userMsg("what is the capital of Kenya?")})

	second := chat(t, baseURL, []map[string]interface{}{
		userMsg("what is the capital of Kenya?"),
		assistantMsg(first),
		userMsg("and its population?"),
	})
	if second == "" {
		t.Fatal("expected a reply on the second turn")
	}

	all := runs()
	if len(all) != 2 {
		t.Fatalf("expected 2 CLI runs, got %d", len(all))
	}

	// Turn one: a fresh session, but persisted so it can be resumed.
	if hasFlag(all[0].Args, "--resume") {
		t.Errorf("first turn must not resume anything: %v", all[0].Args)
	}
	if hasFlag(all[0].Args, "--no-session-persistence") {
		t.Errorf("first turn must persist its session so turn two can resume it: %v", all[0].Args)
	}

	// Turn two: resumes turn one's session id.
	got := flagValue(all[1].Args, "--resume")
	if got != all[0].SessionID {
		t.Errorf("second turn should resume session %q, got --resume %q (args: %v)", all[0].SessionID, got, all[1].Args)
	}
	if hasFlag(all[1].Args, "--no-session-persistence") {
		t.Errorf("resumed turn must keep persisting its session: %v", all[1].Args)
	}

	// And it sends the new user message only — no flattened history.
	if all[1].Stdin != "and its population?" {
		t.Errorf("resumed turn should send only the new user message, got %q", all[1].Stdin)
	}
	if strings.Contains(all[1].Stdin, "[user]:") || strings.Contains(all[1].Stdin, "[assistant]:") {
		t.Errorf("resumed turn must not replay a flattened transcript, got %q", all[1].Stdin)
	}
}

// TestResumeSessionDisabledByDefault pins the default: without the feature
// flag every turn is a fresh, non-persisted CLI process fed the whole history
// as a flattened transcript, exactly as before this feature existed.
func TestResumeSessionDisabledByDefault(t *testing.T) {
	baseURL, runs := newResumeServer(t, Config{})

	first := chat(t, baseURL, []map[string]interface{}{userMsg("what is the capital of Kenya?")})
	chat(t, baseURL, []map[string]interface{}{
		userMsg("what is the capital of Kenya?"),
		assistantMsg(first),
		userMsg("and its population?"),
	})

	all := runs()
	if len(all) != 2 {
		t.Fatalf("expected 2 CLI runs, got %d", len(all))
	}

	for i, run := range all {
		if hasFlag(run.Args, "--resume") {
			t.Errorf("run %d resumed a session with the feature off: %v", i, run.Args)
		}
		if !hasFlag(run.Args, "--no-session-persistence") {
			t.Errorf("run %d must not persist a session with the feature off: %v", i, run.Args)
		}
	}

	if !strings.Contains(all[1].Stdin, "[user]:") || !strings.Contains(all[1].Stdin, "[assistant]:") {
		t.Errorf("second turn should replay the flattened transcript, got %q", all[1].Stdin)
	}
}

// TestResumeSessionFallsBackOnEditedHistory covers the correctness guard: if
// the client rewrites earlier turns, the stored session no longer represents
// the conversation, so the gateway must replay instead of resuming. A later
// turn on the unedited history still resumes.
func TestResumeSessionFallsBackOnEditedHistory(t *testing.T) {
	baseURL, runs := newResumeServer(t, Config{ResumeSessions: true})

	first := chat(t, baseURL, []map[string]interface{}{userMsg("what is the capital of Kenya?")})

	// Same shape, edited first user turn: no stored fingerprint matches.
	chat(t, baseURL, []map[string]interface{}{
		userMsg("what is the capital of Uganda?"),
		assistantMsg(first),
		userMsg("and its population?"),
	})

	// The unedited conversation still resumes turn one's session.
	chat(t, baseURL, []map[string]interface{}{
		userMsg("what is the capital of Kenya?"),
		assistantMsg(first),
		userMsg("and its population?"),
	})

	all := runs()
	if len(all) != 3 {
		t.Fatalf("expected 3 CLI runs, got %d", len(all))
	}

	if hasFlag(all[1].Args, "--resume") {
		t.Errorf("edited history must not resume a session: %v", all[1].Args)
	}
	if !strings.Contains(all[1].Stdin, "[user]:") {
		t.Errorf("edited history should fall back to a flattened replay, got %q", all[1].Stdin)
	}

	if got := flagValue(all[2].Args, "--resume"); got != all[0].SessionID {
		t.Errorf("unedited history should resume session %q, got %q", all[0].SessionID, got)
	}
}

// TestResumeSessionLRUBounded proves the store cannot grow without bound: the
// least recently used conversation is evicted, and evicted conversations fall
// back to replay rather than resuming a stale session.
func TestResumeSessionLRUBounded(t *testing.T) {
	t.Run("store evicts least recently used", func(t *testing.T) {
		store := NewResumeStore(2)

		store.Put("a", "sess-a")
		store.Put("b", "sess-b")
		if _, ok := store.Get("a"); !ok { // "a" is now the most recent
			t.Fatal("expected a hit for a")
		}

		store.Put("c", "sess-c") // evicts "b"

		if store.Len() != 2 {
			t.Errorf("expected 2 entries, got %d", store.Len())
		}
		if _, ok := store.Get("b"); ok {
			t.Error("b should have been evicted as least recently used")
		}
		for _, key := range []string{"a", "c"} {
			if _, ok := store.Get(key); !ok {
				t.Errorf("expected %s to still be resumable", key)
			}
		}
	})

	t.Run("evicted conversation falls back to replay", func(t *testing.T) {
		baseURL, runs := newResumeServer(t, Config{ResumeSessions: true, ResumeMaxEntries: 1})

		firstA := chat(t, baseURL, []map[string]interface{}{userMsg("conversation A")})
		// A second conversation evicts A's entry from the size-1 store.
		chat(t, baseURL, []map[string]interface{}{userMsg("conversation B")})

		chat(t, baseURL, []map[string]interface{}{
			userMsg("conversation A"),
			assistantMsg(firstA),
			userMsg("continue A"),
		})

		all := runs()
		if len(all) != 3 {
			t.Fatalf("expected 3 CLI runs, got %d", len(all))
		}
		if hasFlag(all[2].Args, "--resume") {
			t.Errorf("evicted conversation must not resume: %v", all[2].Args)
		}
		if !strings.Contains(all[2].Stdin, "[user]:") {
			t.Errorf("evicted conversation should replay the transcript, got %q", all[2].Stdin)
		}
	})
}
