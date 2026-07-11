package gateway

import (
	"testing"
)

// TestSupersededSessionReapedImmediately covers the interrupted tool loop:
// the client gets tool_calls back, then the end user interrupts and sends a
// new user turn instead of the tool results. That request cannot resume the
// parked session, so the session must be reaped as the request is served —
// not left holding a MaxSessions slot and a live CLI process until the idle
// sweeper fires (default 600s). MaxSessions is 2 here with two sessions
// already parked, so the superseding request can only be served at all if
// the abandoned session's slot is freed first.
func TestSupersededSessionReapedImmediately(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "relay")
	server, ts := newRelayServer(t, Config{MaxSessions: 2})

	askA := map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}
	askB := map[string]interface{}{"role": "user", "content": "Is it raining in Nairobi?"}

	park := func(ask map[string]interface{}) ToolCall {
		t.Helper()
		status, resp := postChat(t, ts.URL, map[string]interface{}{
			"model":    "sonnet",
			"messages": []interface{}{ask},
			"tools":    weatherTools(),
		})
		if status != 200 || resp.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("expected a parked tool-call turn, got status %d finish %q", status, resp.Choices[0].FinishReason)
		}
		return resp.Choices[0].Message.ToolCalls[0]
	}

	callA := park(askA)
	callB := park(askB)
	if n := server.sessions.GetSessionCount(); n != 2 {
		t.Fatalf("expected 2 parked sessions, got %d", n)
	}

	// Conversation A is interrupted: the history carries A's announced tool
	// call but answers it with a new user turn instead of a tool result.
	status, superseding := postChat(t, ts.URL, map[string]interface{}{
		"model": "sonnet",
		"messages": []interface{}{
			askA,
			map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": toolCallsAsMessages([]ToolCall{callA})},
			map[string]interface{}{"role": "user", "content": "Never mind, what's the weather in Kampala?"},
		},
		"tools": weatherTools(),
	})
	if status != 200 {
		t.Fatalf("superseding request: expected 200 (session A's slot freed), got %d", status)
	}

	if sess := server.sessions.GetSessionByToolCallID(callA.ID); sess != nil {
		t.Errorf("session A should have been reaped, but tool call %s still resolves to session %s", callA.ID, sess.ID)
	}
	if n := server.sessions.GetSessionCount(); n != 2 {
		t.Errorf("expected 2 live sessions (B plus the superseding one), got %d", n)
	}

	// The superseding request opened a session of its own, unrelated to A's.
	newCall := superseding.Choices[0].Message.ToolCalls[0]
	if newCall.ID == callA.ID {
		t.Errorf("superseding turn reused the reaped tool_call_id %s", callA.ID)
	}

	// Session B was never named by that request and must be untouched: still
	// indexed, and its parked CLI process still able to run to a final answer.
	if server.sessions.GetSessionByToolCallID(callB.ID) == nil {
		t.Fatalf("unrelated session B was reaped by A's supersession")
	}
	status, final := postChat(t, ts.URL, map[string]interface{}{
		"model": "sonnet",
		"messages": []interface{}{
			askB,
			map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": toolCallsAsMessages([]ToolCall{callB})},
			map[string]interface{}{"role": "tool", "tool_call_id": callB.ID, "content": "drizzling, 19C"},
		},
		"tools": weatherTools(),
	})
	if status != 200 {
		t.Fatalf("session B continuation: expected 200, got %d", status)
	}
	if got := final.Choices[0].Message.Content; got != "The weather is drizzling, 19C" {
		t.Errorf("session B should have resumed to a final answer, got %v", got)
	}
}
