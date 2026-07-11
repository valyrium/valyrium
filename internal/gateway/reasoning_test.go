package gateway

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func postChatCompletion(t *testing.T, server *Server, req ChatCompletionRequest) (int, map[string]interface{}) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	httpReq := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer test-key")
	httpReq.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httpReq)

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	return w.Code, result
}

// newReasoningTestServer returns a server whose stub CLI records its argv
// to argvFile (one arg per line) before emitting a minimal successful
// response, so tests can assert on the --effort flag the gateway passed.
func newReasoningTestServer(t *testing.T) (server *Server, argvFile string) {
	t.Helper()
	tmpDir := t.TempDir()
	argvFile = filepath.Join(tmpDir, "argv.txt")

	stubBin := filepath.Join(tmpDir, "claude-stub")
	stubScript := `#!/bin/sh
printf '%s\n' "$@" > "` + argvFile + `"
echo '{"type":"result","result":"hello","stop_reason":"end_turn","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
exit 0
`
	if err := os.WriteFile(stubBin, []byte(stubScript), 0755); err != nil {
		t.Fatalf("failed to write stub script: %v", err)
	}

	config := Config{
		Port:         0,
		Host:         "127.0.0.1",
		APIKey:       "test-key",
		DefaultModel: "sonnet",
		Models:       []string{"sonnet"},
		ClaudeBin:    stubBin,
		TimeoutMS:    30000,
		Concurrency:  4,
	}

	return NewServer(config), argvFile
}

// effortFromArgv reads the recorded argv file and returns the value that
// follows a --effort flag, or "" if the flag was never passed.
func effortFromArgv(t *testing.T, argvFile string) string {
	t.Helper()
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("failed to read argv file: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i, a := range args {
		if a == "--effort" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestReasoningObjectMapsToEffort(t *testing.T) {
	server, argvFile := newReasoningTestServer(t)

	reqBody := ChatCompletionRequest{
		Model: "sonnet",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hi"},
		},
		Reasoning: &ReasoningSpec{Effort: "medium"},
	}

	code, _ := postChatCompletion(t, server, reqBody)
	if code != 200 {
		t.Fatalf("expected status 200, got %d", code)
	}

	if got := effortFromArgv(t, argvFile); got != "medium" {
		t.Errorf("expected --effort medium, got %q", got)
	}
}

func TestReasoningEffortPrecedenceOverReasoningObject(t *testing.T) {
	server, argvFile := newReasoningTestServer(t)

	reqBody := ChatCompletionRequest{
		Model: "sonnet",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hi"},
		},
		ReasoningEffort: "high",
		Reasoning:       &ReasoningSpec{Effort: "low"},
	}

	code, _ := postChatCompletion(t, server, reqBody)
	if code != 200 {
		t.Fatalf("expected status 200, got %d", code)
	}

	if got := effortFromArgv(t, argvFile); got != "high" {
		t.Errorf("expected top-level reasoning_effort (high) to win, got %q", got)
	}
}

func TestReasoningDisabledIgnoresEffort(t *testing.T) {
	server, argvFile := newReasoningTestServer(t)

	enabled := false
	reqBody := ChatCompletionRequest{
		Model: "sonnet",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Hi"},
		},
		Reasoning: &ReasoningSpec{Enabled: &enabled, Effort: "high"},
	}

	code, _ := postChatCompletion(t, server, reqBody)
	if code != 200 {
		t.Fatalf("expected status 200, got %d", code)
	}

	if got := effortFromArgv(t, argvFile); got != "" {
		t.Errorf("expected no --effort flag when reasoning is disabled, got %q", got)
	}
}
