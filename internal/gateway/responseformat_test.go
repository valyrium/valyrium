package gateway

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newResponseFormatTestServer(t *testing.T, stubScript string) *Server {
	t.Helper()
	tmpDir := t.TempDir()

	stubBin := filepath.Join(tmpDir, "claude-stub")
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

	return NewServer(config)
}

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

func messageContent(t *testing.T, result map[string]interface{}) string {
	t.Helper()
	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("expected choices in response, got %+v", result)
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected choice to be a map, got %+v", choices[0])
	}
	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message in choice, got %+v", choice)
	}
	content, _ := message["content"].(string)
	return content
}

// TestResponseFormatJSONObjectYieldsJSON covers the "well-behaved model"
// path from issue #9's acceptance criteria: a json_object request whose
// first response is already valid JSON is returned as-is.
func TestResponseFormatJSONObjectYieldsJSON(t *testing.T) {
	stubScript := `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"answer\":\"hi\"}"}}}'
echo '{"type":"result","result":"{\"answer\":\"hi\"}","stop_reason":"end_turn","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
exit 0
`
	server := newResponseFormatTestServer(t, stubScript)

	code, result := postChatCompletion(t, server, ChatCompletionRequest{
		Model:          "sonnet",
		Messages:       []OpenAIMessage{{Role: "user", Content: "Reply with JSON"}},
		ResponseFormat: &ResponseFormat{Type: "json_object"},
	})

	if code != 200 {
		t.Fatalf("expected status 200, got %d: %+v", code, result)
	}

	content := messageContent(t, result)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("expected message content to be parseable JSON, got %q: %v", content, err)
	}
	if parsed["answer"] != "hi" {
		t.Errorf("expected answer=hi, got %+v", parsed)
	}
}

// TestResponseFormatJSONSchemaIncludesSchemaInPrompt covers the second
// acceptance bullet: a json_schema request's schema shows up in the
// instruction handed to the CLI (via --system-prompt).
func TestResponseFormatJSONSchemaIncludesSchemaInPrompt(t *testing.T) {
	tmpDir := t.TempDir()
	promptCapture := filepath.Join(tmpDir, "system-prompt.txt")

	stubScript := `#!/bin/sh
for i in "$@"; do
  if [ "$prev" = "--system-prompt" ]; then
    printf '%s' "$i" > "` + promptCapture + `"
  fi
  prev="$i"
done
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"answer\":42}"}}}'
echo '{"type":"result","result":"{\"answer\":42}","stop_reason":"end_turn","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
exit 0
`
	server := newResponseFormatTestServer(t, stubScript)

	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"answer": map[string]interface{}{"type": "number"},
		},
		"required": []interface{}{"answer"},
	}

	code, result := postChatCompletion(t, server, ChatCompletionRequest{
		Model:    "sonnet",
		Messages: []OpenAIMessage{{Role: "user", Content: "Reply with JSON"}},
		ResponseFormat: &ResponseFormat{
			Type: "json_schema",
			JSONSchema: &JSONSchemaSpec{
				Name:   "answer_schema",
				Schema: schema,
			},
		},
	})

	if code != 200 {
		t.Fatalf("expected status 200, got %d: %+v", code, result)
	}

	captured, err := os.ReadFile(promptCapture)
	if err != nil {
		t.Fatalf("failed to read captured system prompt: %v", err)
	}

	schemaJSON, _ := json.Marshal(schema)
	if !bytes.Contains(captured, schemaJSON) {
		t.Errorf("expected system prompt to contain schema %s, got %s", schemaJSON, captured)
	}
}

// TestResponseFormatRetriesOnInvalidJSON covers the third acceptance
// bullet: when the model's first reply is not valid JSON, the gateway
// retries once with the error appended, and returns the corrected attempt.
func TestResponseFormatRetriesOnInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "call-count")

	stubScript := `#!/bin/sh
count=0
if [ -f "` + counterFile + `" ]; then
  count=$(cat "` + counterFile + `")
fi
count=$((count + 1))
echo "$count" > "` + counterFile + `"

if [ "$count" -eq 1 ]; then
  echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"sure, here you go: not json"}}}'
  echo '{"type":"result","result":"sure, here you go: not json","stop_reason":"end_turn","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
else
  echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"ok\":true}"}}}'
  echo '{"type":"result","result":"{\"ok\":true}","stop_reason":"end_turn","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
fi
exit 0
`
	server := newResponseFormatTestServer(t, stubScript)

	code, result := postChatCompletion(t, server, ChatCompletionRequest{
		Model:          "sonnet",
		Messages:       []OpenAIMessage{{Role: "user", Content: "Reply with JSON"}},
		ResponseFormat: &ResponseFormat{Type: "json_object"},
	})

	if code != 200 {
		t.Fatalf("expected status 200, got %d: %+v", code, result)
	}

	content := messageContent(t, result)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("expected retried message content to be parseable JSON, got %q: %v", content, err)
	}
	if parsed["ok"] != true {
		t.Errorf("expected ok=true, got %+v", parsed)
	}

	callCountBytes, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("failed to read call counter: %v", err)
	}
	if got := string(bytes.TrimSpace(callCountBytes)); got != "2" {
		t.Errorf("expected the stub CLI to be invoked exactly twice, got %s", got)
	}
}
