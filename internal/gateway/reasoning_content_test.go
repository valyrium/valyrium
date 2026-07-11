package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestReasoningHiddenByDefault proves that with CLAUDE_GATEWAY_EXPOSE_REASONING
// unset (the default), thinking deltas from the CLI stream are dropped:
// reasoning_content never appears on the message, in an SSE delta, or
// bleeding into the visible content (issue #15).
func TestReasoningHiddenByDefault(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "reasoning")
	_, ts := newRelayServer(t, Config{})

	userMsg := map[string]interface{}{"role": "user", "content": "What is the answer?"}

	status, resp := postChat(t, ts.URL, map[string]interface{}{
		"model":    "sonnet",
		"messages": []interface{}{userMsg},
	})
	if status != 200 {
		t.Fatalf("expected 200, got %d", status)
	}
	choice := resp.Choices[0]
	if choice.Message.ReasoningContent != "" {
		t.Errorf("expected no reasoning_content by default, got %q", choice.Message.ReasoningContent)
	}
	if choice.Message.Content != "The answer is 42" {
		t.Errorf("expected content unaffected, got %v", choice.Message.Content)
	}

	t.Run("streaming", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]interface{}{
			"model":    "sonnet",
			"stream":   true,
			"messages": []interface{}{userMsg},
		})
		client := &http.Client{Timeout: 15 * time.Second}
		httpResp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = httpResp.Body.Close() }()
		raw, _ := io.ReadAll(httpResp.Body)
		if strings.Contains(string(raw), "reasoning_content") {
			t.Errorf("reasoning_content leaked into SSE stream by default: %s", raw)
		}
	})
}

// TestReasoningContentExposedWhenFlagOn proves that with the flag on,
// thinking deltas surface as reasoning_content — on the final message for
// non-streaming requests, and on delta chunks (excluded from content) for
// streaming requests — while usage accounting is unaffected (issue #15).
func TestReasoningContentExposedWhenFlagOn(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "reasoning")
	_, ts := newRelayServer(t, Config{ExposeReasoning: true})

	userMsg := map[string]interface{}{"role": "user", "content": "What is the answer?"}

	status, resp := postChat(t, ts.URL, map[string]interface{}{
		"model":    "sonnet",
		"messages": []interface{}{userMsg},
	})
	if status != 200 {
		t.Fatalf("expected 200, got %d", status)
	}
	choice := resp.Choices[0]
	if choice.Message.ReasoningContent != "Let me think about this." {
		t.Errorf("expected reasoning_content to carry the thinking delta, got %q", choice.Message.ReasoningContent)
	}
	if choice.Message.Content != "The answer is 42" {
		t.Errorf("expected reasoning excluded from content, got %v", choice.Message.Content)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 6 {
		t.Errorf("expected usage accounting unaffected, got %+v", resp.Usage)
	}

	t.Run("streaming", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]interface{}{
			"model":    "sonnet",
			"stream":   true,
			"messages": []interface{}{userMsg},
		})
		client := &http.Client{Timeout: 15 * time.Second}
		httpResp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = httpResp.Body.Close() }()
		raw, _ := io.ReadAll(httpResp.Body)
		body := string(raw)
		if !strings.Contains(body, `"reasoning_content":"Let me think about this."`) {
			t.Errorf("expected a reasoning_content delta chunk, got: %s", body)
		}
		if !strings.Contains(body, `"content":"The answer is 42"`) {
			t.Errorf("expected the text delta chunk to carry only visible content, got: %s", body)
		}
	})
}
