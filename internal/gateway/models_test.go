package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestModelsIncludeContextLength(t *testing.T) {
	config := Config{
		Port:         0,
		Host:         "127.0.0.1",
		APIKey:       "test-key",
		DefaultModel: "sonnet",
		Models:       []string{"sonnet", "opus", "haiku", "some-custom-model"},
	}
	server := NewServer(config)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result ModelsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	for _, model := range result.Data {
		if model.ContextLength == 0 {
			t.Errorf("model %q: expected non-zero context_length", model.ID)
		}
		if model.MaxModelLen != model.ContextLength {
			t.Errorf("model %q: expected max_model_len == context_length (%d), got %d", model.ID, model.ContextLength, model.MaxModelLen)
		}
	}

	byID := make(map[string]ModelInfo)
	for _, model := range result.Data {
		byID[model.ID] = model
	}

	for _, id := range []string{"sonnet", "opus", "haiku"} {
		if byID[id].ContextLength != 200000 {
			t.Errorf("model %q: expected context_length 200000, got %d", id, byID[id].ContextLength)
		}
	}

	if byID["some-custom-model"].ContextLength != defaultContextLength {
		t.Errorf("unknown model: expected default context_length %d, got %d", defaultContextLength, byID["some-custom-model"].ContextLength)
	}
}

func TestModelsContextLengthOverride(t *testing.T) {
	config := Config{
		Port:         0,
		Host:         "127.0.0.1",
		APIKey:       "test-key",
		DefaultModel: "sonnet",
		Models:       []string{"sonnet", "opus", "some-custom-model"},
		ContextLengths: map[string]int{
			"sonnet":            1000000,
			"some-custom-model": 32000,
		},
		DefaultContextLength: 64000,
	}
	server := NewServer(config)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result ModelsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	byID := make(map[string]ModelInfo)
	for _, model := range result.Data {
		byID[model.ID] = model
	}

	if byID["sonnet"].ContextLength != 1000000 {
		t.Errorf("sonnet: expected overridden context_length 1000000, got %d", byID["sonnet"].ContextLength)
	}
	if byID["some-custom-model"].ContextLength != 32000 {
		t.Errorf("some-custom-model: expected overridden context_length 32000, got %d", byID["some-custom-model"].ContextLength)
	}
	// opus has no override but matches the known family, which takes
	// priority over the configured default.
	if byID["opus"].ContextLength != 200000 {
		t.Errorf("opus: expected known-family context_length 200000, got %d", byID["opus"].ContextLength)
	}
}
