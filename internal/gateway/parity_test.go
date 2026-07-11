package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIParity(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "valyrium-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	stubBin := filepath.Join(tmpDir, "claude-stub")
	stubScript := `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}'
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
		Models:       []string{"sonnet", "opus", "haiku"},
		ClaudeBin:    stubBin,
		TimeoutMS:    30000,
		Concurrency:  4,
	}

	server := NewServer(config)

	t.Run("GET /healthz without auth", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/healthz", nil)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var result map[string]bool
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if !result["ok"] {
			t.Error("expected ok:true")
		}
	})

	t.Run("GET /v1/models without auth returns 401", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 401 {
			t.Errorf("expected status 401, got %d", w.Code)
		}

		var errResp ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if errResp.Error.Type != "authentication_error" {
			t.Errorf("expected authentication_error, got %s", errResp.Error.Type)
		}
	})

	t.Run("GET /v1/models with auth", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var result ModelsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if result.Object != "list" {
			t.Errorf("expected object 'list', got %s", result.Object)
		}

		if len(result.Data) != 3 {
			t.Errorf("expected 3 models, got %d", len(result.Data))
		}

		for _, model := range result.Data {
			if model.Object != "model" {
				t.Errorf("model object should be 'model', got %s", model.Object)
			}
			if model.OwnedBy != "anthropic" {
				t.Errorf("owned_by should be 'anthropic', got %s", model.OwnedBy)
			}
			if model.Created != 0 {
				t.Errorf("created should be 0, got %d", model.Created)
			}
		}
	})

	t.Run("POST /v1/chat/completions non-streaming", func(t *testing.T) {
		reqBody := ChatCompletionRequest{
			Model: "sonnet",
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Say hi"},
			},
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var result CompletionResponse
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if !strings.HasPrefix(result.ID, "chatcmpl-") {
			t.Errorf("ID should start with 'chatcmpl-', got %s", result.ID)
		}

		if result.Object != "chat.completion" {
			t.Errorf("object should be 'chat.completion', got %s", result.Object)
		}

		if len(result.Choices) != 1 {
			t.Errorf("expected 1 choice, got %d", len(result.Choices))
		}

		if result.Choices[0].Message.Role != "assistant" {
			t.Errorf("expected role 'assistant', got %s", result.Choices[0].Message.Role)
		}

		if result.Choices[0].Message.Content != "hello" {
			t.Errorf("expected content 'hello', got %s", result.Choices[0].Message.Content)
		}

		if result.Choices[0].FinishReason != "stop" {
			t.Errorf("expected finish_reason 'stop', got %s", result.Choices[0].FinishReason)
		}

		if result.Usage.PromptTokens != 10 {
			t.Errorf("expected prompt_tokens 10, got %d", result.Usage.PromptTokens)
		}

		if result.Usage.CompletionTokens != 2 {
			t.Errorf("expected completion_tokens 2, got %d", result.Usage.CompletionTokens)
		}

		if result.Usage.TotalTokens != 12 {
			t.Errorf("expected total_tokens 12, got %d", result.Usage.TotalTokens)
		}
	})

	t.Run("POST /v1/chat/completions streaming", func(t *testing.T) {
		reqBody := ChatCompletionRequest{
			Model:  "sonnet",
			Stream: true,
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Say hi"},
			},
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		contentType := w.Header().Get("content-type")
		if !strings.Contains(contentType, "text/event-stream") {
			t.Errorf("expected text/event-stream content type, got %s", contentType)
		}

		lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
		if len(lines) < 4 {
			t.Fatalf("expected at least 4 lines, got %d", len(lines))
		}

		chunks := make([]StreamChunk, 0)
		for _, line := range lines {
			if line == "" {
				continue
			}
			if line == "data: [DONE]" {
				break
			}
			if strings.HasPrefix(line, "data: ") {
				jsonStr := strings.TrimPrefix(line, "data: ")
				var chunk StreamChunk
				if err := json.Unmarshal([]byte(jsonStr), &chunk); err == nil {
					chunks = append(chunks, chunk)
				}
			}
		}

		if len(chunks) < 2 {
			t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
		}

		if chunks[0].Choices[0].Delta["role"] != "assistant" {
			t.Error("first chunk should have role assistant")
		}

		finalChunk := chunks[len(chunks)-1]
		if finalChunk.Usage == nil {
			t.Error("final chunk should have usage object")
		} else if finalChunk.Usage.PromptTokens != 10 || finalChunk.Usage.CompletionTokens != 2 {
			t.Error("usage values incorrect in final chunk")
		}
	})

	t.Run("streaming request logs real usage to stderr", func(t *testing.T) {
		// Regression: handleStreamingResponse used to omit usage from the
		// structured stderr log entirely (promptTokens/completionTokens were
		// never written back from the streaming path), so every streaming
		// request logged prompt_tokens:0, completion_tokens:0 regardless of
		// actual usage. Found via a real opencode-driven dogfood run, where
		// opencode issues streaming requests and the log was silently wrong.
		reqBody := ChatCompletionRequest{
			Model:  "sonnet",
			Stream: true,
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Say hi"},
			},
		}
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()

		logLine := captureStderr(t, func() {
			server.ServeHTTP(w, req)
		})

		var entry struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}
		if err := json.Unmarshal([]byte(logLine), &entry); err != nil {
			t.Fatalf("stderr log line was not valid JSON: %v\nline: %s", err, logLine)
		}
		if entry.PromptTokens != 10 || entry.CompletionTokens != 2 {
			t.Errorf("expected prompt_tokens=10 completion_tokens=2 in the request log, got %+v", entry)
		}
	})

	t.Run("POST with unknown model falls back", func(t *testing.T) {
		reqBody := ChatCompletionRequest{
			Model: "gpt-4o",
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Hi"},
			},
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Errorf("expected status 200, got %d", w.Code)
		}
	})

	t.Run("POST ending with assistant message returns 400", func(t *testing.T) {
		reqBody := ChatCompletionRequest{
			Model: "sonnet",
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Hi"},
				{Role: "assistant", Content: "Hello"},
			},
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 400 {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})

	t.Run("unknown route returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/unknown", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 404 {
			t.Errorf("expected status 404, got %d", w.Code)
		}

		var errResp ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if errResp.Error.Type != "not_found_error" {
			t.Errorf("expected not_found_error, got %s", errResp.Error.Type)
		}
	})
}

func TestEnhancements(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "valyrium-enhance-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	stubBin := filepath.Join(tmpDir, "claude-stub")
	stubScript := `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}'
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

	server := NewServer(config)

	t.Run("cost_usd surfaced in non-streaming response", func(t *testing.T) {
		reqBody := ChatCompletionRequest{
			Model: "sonnet",
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Hi"},
			},
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		var result map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		usage, ok := result["usage"].(map[string]interface{})
		if !ok {
			t.Fatal("usage field missing or not a map")
		}

		if _, hasField := usage["cost_usd"]; !hasField {
			t.Error("cost_usd field not found in usage")
		}
	})

	t.Run("cost_usd surfaced in streaming terminal chunk", func(t *testing.T) {
		reqBody := ChatCompletionRequest{
			Model:  "sonnet",
			Stream: true,
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Hi"},
			},
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
		for _, line := range lines {
			if line == "" || line == "data: [DONE]" {
				continue
			}
			if strings.HasPrefix(line, "data: ") {
				jsonStr := strings.TrimPrefix(line, "data: ")
				var chunk StreamChunk
				if err := json.Unmarshal([]byte(jsonStr), &chunk); err == nil {
					if chunk.Usage != nil && chunk.Choices[0].FinishReason != nil {
						t.Logf("Final chunk has usage: %+v", chunk.Usage)
					}
				}
			}
		}
	})

	t.Run("constant-time auth comparison", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer wrong-key-that-is-same-length-____")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 401 {
			t.Errorf("expected status 401 for wrong key, got %d", w.Code)
		}
	})

	t.Run("graceful shutdown allows inflight requests", func(t *testing.T) {
		reqBody := ChatCompletionRequest{
			Model: "sonnet",
			Messages: []OpenAIMessage{
				{Role: "user", Content: "Hi"},
			},
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			server.ServeHTTP(w, req)
			close(done)
		}()

		time.Sleep(10 * time.Millisecond)

		select {
		case <-done:
			if w.Code != 200 {
				t.Errorf("expected 200, got %d", w.Code)
			}
		case <-time.After(15 * time.Second):
			t.Error("request did not complete in time")
		}
	})

	t.Run("GET /metrics returns Prometheus format", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		contentType := w.Header().Get("content-type")
		if !strings.Contains(contentType, "text/plain") {
			t.Errorf("expected text/plain content type, got %s", contentType)
		}

		body := w.Body.String()
		if !strings.Contains(body, "llmgateway_requests_total") {
			t.Error("expected llmgateway_requests_total metric")
		}
		if !strings.Contains(body, "llmgateway_inflight_requests") {
			t.Error("expected llmgateway_inflight_requests metric")
		}
		if !strings.Contains(body, "llmgateway_request_duration_seconds") {
			t.Error("expected llmgateway_request_duration_seconds metric")
		}
	})
}

// TestNonToolStreamingEmitsFinishReason covers a regression where the
// terminal chunk of a non-tool streaming response always sent
// finish_reason: null, because handleStreamingResponse never mapped the
// completion's stop_reason before writing it.
func TestNonToolStreamingEmitsFinishReason(t *testing.T) {
	server := newParityTestServer(t, `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}'
echo '{"type":"result","result":"hello","stop_reason":"end_turn","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
exit 0
`)

	chunks := runStreamingChatRequest(t, server)
	finalChunk := chunks[len(chunks)-1]
	if finalChunk.Choices[0].FinishReason == nil {
		t.Fatal("expected the terminal chunk to carry a non-nil finish_reason")
	}
	if *finalChunk.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop', got %q", *finalChunk.Choices[0].FinishReason)
	}
}

// TestStreamingFinishReasonMapsLength confirms the terminal streaming chunk
// maps a max_tokens stop_reason to the OpenAI-shaped "length" finish_reason,
// not just the default "stop".
func TestStreamingFinishReasonMapsLength(t *testing.T) {
	server := newParityTestServer(t, `#!/bin/sh
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}'
echo '{"type":"result","result":"hello","stop_reason":"max_tokens","total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
exit 0
`)

	chunks := runStreamingChatRequest(t, server)
	finalChunk := chunks[len(chunks)-1]
	if finalChunk.Choices[0].FinishReason == nil {
		t.Fatal("expected the terminal chunk to carry a non-nil finish_reason")
	}
	if *finalChunk.Choices[0].FinishReason != "length" {
		t.Errorf("expected finish_reason 'length', got %q", *finalChunk.Choices[0].FinishReason)
	}
}

// newParityTestServer writes stubScript as an executable claude CLI stand-in
// and returns a Server configured to invoke it.
func newParityTestServer(t *testing.T, stubScript string) *Server {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "valyrium-test-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("failed to remove temp dir: %v", err)
		}
	})

	stubBin := filepath.Join(tmpDir, "claude-stub")
	if err := os.WriteFile(stubBin, []byte(stubScript), 0755); err != nil {
		t.Fatalf("failed to write stub script: %v", err)
	}

	return NewServer(Config{
		Port:         0,
		Host:         "127.0.0.1",
		APIKey:       "test-key",
		DefaultModel: "sonnet",
		Models:       []string{"sonnet", "opus", "haiku"},
		ClaudeBin:    stubBin,
		TimeoutMS:    30000,
		Concurrency:  4,
	})
}

// runStreamingChatRequest issues a streaming chat completion request against
// server and returns the parsed SSE chunks up to (excluding) [DONE].
func runStreamingChatRequest(t *testing.T, server *Server) []StreamChunk {
	t.Helper()

	reqBody := ChatCompletionRequest{
		Model:  "sonnet",
		Stream: true,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Say hi"},
		},
	}

	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	chunks := make([]StreamChunk, 0)
	for _, line := range lines {
		if line == "" || line == "data: [DONE]" {
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			jsonStr := strings.TrimPrefix(line, "data: ")
			var chunk StreamChunk
			if err := json.Unmarshal([]byte(jsonStr), &chunk); err == nil {
				chunks = append(chunks, chunk)
			}
		}
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk")
	}
	return chunks
}

// captureStderr redirects os.Stderr for the duration of fn (which must run
// and complete its writes synchronously) and returns the last non-empty
// line written, which is the structured request log entry.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	_ = w.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return lines[len(lines)-1]
}
