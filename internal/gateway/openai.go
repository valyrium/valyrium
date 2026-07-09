package gateway

import (
	"fmt"
	"strings"
)

type OpenAIMessage struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	ToolCallID string      `json:"tool_call_id"`
	ToolCalls  []ToolCall  `json:"tool_calls"`
}

type ToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function ToolCallFunction       `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolSchema struct {
	Type     string                 `json:"type"`
	Function ToolFunctionSchema     `json:"function"`
}

type ToolFunctionSchema struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type ChatCompletionRequest struct {
	Model             string           `json:"model"`
	Messages          []OpenAIMessage  `json:"messages"`
	Tools             []ToolSchema     `json:"tools"`
	ToolChoice        interface{}      `json:"tool_choice"`
	Stream            bool             `json:"stream"`
	ReasoningEffort   string           `json:"reasoning_effort"`
	Temperature       float64          `json:"temperature"`
	TopP              float64          `json:"top_p"`
	MaxTokens         int              `json:"max_tokens"`
	MaxCompletionTokens int            `json:"max_completion_tokens"`
}

type TextContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type Completion struct {
	Text       string
	Model      string
	StopReason *string
	Usage      Usage
	CostUSD    *float64
}

const defaultSystemPrompt = "You are a helpful assistant."
const transcriptInstruction = `The user message contains a conversation transcript with turns marked "[user]:" and "[assistant]:". Continue the conversation by writing the next assistant reply to the final user turn. Output only the reply itself, with no role marker or commentary about the transcript.`

func textOf(msg OpenAIMessage) (string, error) {
	if msg.Content == nil {
		return "", nil
	}

	if s, ok := msg.Content.(string); ok {
		return s, nil
	}

	if arr, ok := msg.Content.([]interface{}); ok {
		var parts []string
		for _, part := range arr {
			m, ok := part.(map[string]interface{})
			if !ok {
				return "", fmt.Errorf("only text content parts are supported by this gateway")
			}
			if t, ok := m["type"].(string); ok && t == "text" {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
					continue
				}
			}
			return "", fmt.Errorf("only text content parts are supported by this gateway")
		}
		return strings.Join(parts, ""), nil
	}

	return "", fmt.Errorf("only text content parts are supported by this gateway")
}

func BuildPrompt(messages []OpenAIMessage) (systemPrompt, prompt string, err error) {
	if len(messages) == 0 {
		return "", "", fmt.Errorf("messages must be a non-empty array")
	}

	var systemParts []string
	type turn struct {
		role string
		text string
	}
	var turns []turn

	for _, msg := range messages {
		text, err := textOf(msg)
		if err != nil {
			return "", "", err
		}

		switch msg.Role {
		case "system", "developer":
			systemParts = append(systemParts, text)
		case "user":
			turns = append(turns, turn{role: "user", text: text})
		case "assistant":
			turns = append(turns, turn{role: "assistant", text: text})
		default:
			return "", "", fmt.Errorf("unsupported message role: %s", msg.Role)
		}
	}

	if len(turns) == 0 || turns[len(turns)-1].role != "user" {
		return "", "", fmt.Errorf("the last non-system message must be a user message")
	}

	systemPrompt = strings.Join(systemParts, "\n\n")
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}

	if len(turns) == 1 {
		return systemPrompt, turns[0].text, nil
	}

	systemPrompt += "\n\n" + transcriptInstruction
	var turnStrs []string
	for _, t := range turns {
		turnStrs = append(turnStrs, fmt.Sprintf("[%s]: %s", t.role, t.text))
	}
	prompt = strings.Join(turnStrs, "\n\n")

	return systemPrompt, prompt, nil
}

func MapFinishReason(stopReason *string) string {
	if stopReason == nil {
		return "stop"
	}
	switch *stopReason {
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

type OpenAIUsage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
}

func ToOpenAIUsage(usage Usage, costUSD *float64) OpenAIUsage {
	promptTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	return OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      promptTokens + usage.OutputTokens,
		CostUSD:          costUSD,
	}
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Logprobs     *string `json:"logprobs"`
}

type CompletionResponse struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []Choice      `json:"choices"`
	Usage   OpenAIUsage   `json:"usage"`
}

func CompletionResponseWithCost(id string, created int64, completion Completion) CompletionResponse {
	return CompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   completion.Model,
		Choices: []Choice{
			{
				Index: 0,
				Message: Message{
					Role:    "assistant",
					Content: completion.Text,
				},
				FinishReason: MapFinishReason(completion.StopReason),
				Logprobs:     nil,
			},
		},
		Usage: ToOpenAIUsage(completion.Usage, completion.CostUSD),
	}
}

type StreamChunkDelta map[string]interface{}

type StreamChunkChoice struct {
	Index        int                    `json:"index"`
	Delta        StreamChunkDelta       `json:"delta"`
	FinishReason *string                `json:"finish_reason"`
	Logprobs     *string                `json:"logprobs"`
}

type StreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []StreamChunkChoice  `json:"choices"`
	Usage   *OpenAIUsage         `json:"usage,omitempty"`
}

func NewStreamChunk(id string, created int64, model string, delta StreamChunkDelta, finishReason *string, usage *OpenAIUsage) StreamChunk {
	return StreamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []StreamChunkChoice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: finishReason,
				Logprobs:     nil,
			},
		},
		Usage: usage,
	}
}

type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    *string `json:"code"`
		Param   *string `json:"param"`
	} `json:"error"`
}

type ModelsResponse struct {
	Object string `json:"object"`
	Data   []ModelInfo `json:"data"`
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	OwnedBy string `json:"owned_by"`
}
