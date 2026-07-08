package gateway

import (
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	tests := []struct {
		name          string
		messages      []OpenAIMessage
		expectSystem  string
		expectPrompt  string
		expectErr     bool
	}{
		{
			name:      "empty messages",
			messages:  []OpenAIMessage{},
			expectErr: true,
		},
		{
			name: "single user message",
			messages: []OpenAIMessage{
				{Role: "user", Content: "Hello"},
			},
			expectSystem: defaultSystemPrompt,
			expectPrompt: "Hello",
		},
		{
			name: "system and user message",
			messages: []OpenAIMessage{
				{Role: "system", Content: "You are helpful"},
				{Role: "user", Content: "Hello"},
			},
			expectSystem: "You are helpful",
			expectPrompt: "Hello",
		},
		{
			name: "developer and user message",
			messages: []OpenAIMessage{
				{Role: "developer", Content: "Be precise"},
				{Role: "user", Content: "Hello"},
			},
			expectSystem: "Be precise",
			expectPrompt: "Hello",
		},
		{
			name: "multiple system messages join with newlines",
			messages: []OpenAIMessage{
				{Role: "system", Content: "First"},
				{Role: "system", Content: "Second"},
				{Role: "user", Content: "Hello"},
			},
			expectSystem: "First\n\nSecond",
			expectPrompt: "Hello",
		},
		{
			name: "multi-turn conversation",
			messages: []OpenAIMessage{
				{Role: "user", Content: "First question"},
				{Role: "assistant", Content: "First answer"},
				{Role: "user", Content: "Follow up"},
			},
			expectSystem: defaultSystemPrompt + "\n\n" + transcriptInstruction,
			expectPrompt: "[user]: First question\n\n[assistant]: First answer\n\n[user]: Follow up",
		},
		{
			name: "no system message defaults",
			messages: []OpenAIMessage{
				{Role: "user", Content: "Hello"},
			},
			expectSystem: defaultSystemPrompt,
			expectPrompt: "Hello",
		},
		{
			name: "null content treated as empty",
			messages: []OpenAIMessage{
				{Role: "user", Content: nil},
			},
			expectSystem: defaultSystemPrompt,
			expectPrompt: "",
		},
		{
			name: "text content part array",
			messages: []OpenAIMessage{
				{Role: "user", Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "Hello"},
				}},
			},
			expectSystem: defaultSystemPrompt,
			expectPrompt: "Hello",
		},
		{
			name: "multiple text parts joined",
			messages: []OpenAIMessage{
				{Role: "user", Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "Hello"},
					map[string]interface{}{"type": "text", "text": " world"},
				}},
			},
			expectSystem: defaultSystemPrompt,
			expectPrompt: "Hello world",
		},
		{
			name: "assistant as last message fails",
			messages: []OpenAIMessage{
				{Role: "user", Content: "Hello"},
				{Role: "assistant", Content: "Hi"},
			},
			expectErr: true,
		},
		{
			name: "tool role unsupported",
			messages: []OpenAIMessage{
				{Role: "tool", Content: "{}"},
			},
			expectErr: true,
		},
		{
			name: "non-text content part fails",
			messages: []OpenAIMessage{
				{Role: "user", Content: []interface{}{
					map[string]interface{}{"type": "image", "url": "http://example.com"},
				}},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			system, prompt, err := BuildPrompt(tt.messages)
			if (err != nil) != tt.expectErr {
				t.Fatalf("expected error %v, got %v", tt.expectErr, err)
			}
			if err != nil {
				return
			}
			if system != tt.expectSystem {
				t.Errorf("system prompt mismatch:\nexpected: %q\ngot: %q", tt.expectSystem, system)
			}
			if prompt != tt.expectPrompt {
				t.Errorf("prompt mismatch:\nexpected: %q\ngot: %q", tt.expectPrompt, prompt)
			}
		})
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		input  *string
		expect string
	}{
		{nil, "stop"},
		{ptr("max_tokens"), "length"},
		{ptr("refusal"), "content_filter"},
		{ptr("end_turn"), "stop"},
		{ptr("stop_sequence"), "stop"},
		{ptr(""), "stop"},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := MapFinishReason(tt.input)
			if got != tt.expect {
				t.Errorf("expected %q, got %q", tt.expect, got)
			}
		})
	}
}

func TestToOpenAIUsage(t *testing.T) {
	usage := Usage{
		InputTokens:              10,
		OutputTokens:             5,
		CacheReadInputTokens:     2,
		CacheCreationInputTokens: 3,
	}
	cost := 0.001

	result := ToOpenAIUsage(usage, &cost)

	if result.PromptTokens != 15 { // 10 + 2 + 3
		t.Errorf("prompt_tokens: expected 15, got %d", result.PromptTokens)
	}
	if result.CompletionTokens != 5 {
		t.Errorf("completion_tokens: expected 5, got %d", result.CompletionTokens)
	}
	if result.TotalTokens != 20 { // 15 + 5
		t.Errorf("total_tokens: expected 20, got %d", result.TotalTokens)
	}
	if result.CostUSD == nil || *result.CostUSD != 0.001 {
		t.Errorf("cost_usd: expected 0.001, got %v", result.CostUSD)
	}
}

func ptr[T any](v T) *T {
	return &v
}
