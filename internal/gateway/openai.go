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
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolSchema struct {
	Type     string             `json:"type"`
	Function ToolFunctionSchema `json:"function"`
}

type ToolFunctionSchema struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type ChatCompletionRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	Tools               []ToolSchema    `json:"tools"`
	ToolChoice          interface{}     `json:"tool_choice"`
	Stream              bool            `json:"stream"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	Temperature         float64         `json:"temperature"`
	TopP                float64         `json:"top_p"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Reasoning           *ReasoningSpec  `json:"reasoning"`
	ResponseFormat      *ResponseFormat `json:"response_format"`
}

// ReasoningSpec mirrors the OpenRouter-style extra-body `reasoning` object
// some OpenAI-compatible clients send instead of (or alongside) the
// top-level `reasoning_effort` field. `reasoning_effort` takes precedence
// when both are present.
type ReasoningSpec struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

// ResponseFormat mirrors OpenAI's response_format request field. Only
// "json_object" and "json_schema" are meaningful here; "text" (or an absent
// field) leaves the model free-form.
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

type JSONSchemaSpec struct {
	Name   string                 `json:"name"`
	Schema map[string]interface{} `json:"schema"`
	Strict *bool                  `json:"strict,omitempty"`
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
const toolTranscriptInstruction = `Turns marked "[assistant called tool <name> (<id>)]" and "[tool <name> (<id>) returned]" record tool calls the assistant made earlier and the results those tools produced; treat them as completed context when writing the reply.`

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
		kind     string // "user" | "assistant" | "tool"
		raw      string
		rendered string
	}
	var turns []turn
	toolNames := make(map[string]string)
	hasToolTurns := false

	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			text, err := textOf(msg)
			if err != nil {
				return "", "", err
			}
			systemParts = append(systemParts, text)
		case "user":
			text, err := textOf(msg)
			if err != nil {
				return "", "", err
			}
			turns = append(turns, turn{kind: "user", raw: text, rendered: fmt.Sprintf("[user]: %s", text)})
		case "assistant":
			text, err := textOf(msg)
			if err != nil {
				return "", "", err
			}
			if text != "" || len(msg.ToolCalls) == 0 {
				turns = append(turns, turn{kind: "assistant", raw: text, rendered: fmt.Sprintf("[assistant]: %s", text)})
			}
			// Cold-history flattening: serialize tool calls the gateway
			// has no live session for as transcript markers (ADR 0001 §3).
			for _, tc := range msg.ToolCalls {
				toolNames[tc.ID] = tc.Function.Name
				turns = append(turns, turn{
					kind:     "tool",
					rendered: fmt.Sprintf("[assistant called tool %s (%s)]: %s", tc.Function.Name, tc.ID, tc.Function.Arguments),
				})
				hasToolTurns = true
			}
		case "tool":
			if msg.ToolCallID == "" {
				return "", "", fmt.Errorf("tool messages must carry a tool_call_id")
			}
			text, err := textOf(msg)
			if err != nil {
				return "", "", err
			}
			name := toolNames[msg.ToolCallID]
			if name == "" {
				name = "unknown"
			}
			turns = append(turns, turn{
				kind:     "tool",
				rendered: fmt.Sprintf("[tool %s (%s) returned]: %s", name, msg.ToolCallID, text),
			})
			hasToolTurns = true
		default:
			return "", "", fmt.Errorf("unsupported message role: %s", msg.Role)
		}
	}

	if len(turns) == 0 || turns[len(turns)-1].kind == "assistant" {
		return "", "", fmt.Errorf("the last non-system message must be a user message")
	}

	systemPrompt = strings.Join(systemParts, "\n\n")
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}

	if len(turns) == 1 && turns[0].kind == "user" {
		return systemPrompt, turns[0].raw, nil
	}

	systemPrompt += "\n\n" + transcriptInstruction
	if hasToolTurns {
		systemPrompt += " " + toolTranscriptInstruction
	}
	var turnStrs []string
	for _, t := range turns {
		turnStrs = append(turnStrs, t.rendered)
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
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []Choice    `json:"choices"`
	Usage   OpenAIUsage `json:"usage"`
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

// ToolCallMessage is the assistant message shape for a turn that proposes
// tool calls: content is null (or the text produced alongside), and
// tool_calls carries the gateway-minted call ids.
type ToolCallMessage struct {
	Role      string      `json:"role"`
	Content   interface{} `json:"content"`
	ToolCalls []ToolCall  `json:"tool_calls"`
}

type ToolCallChoice struct {
	Index        int             `json:"index"`
	Message      ToolCallMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     *string         `json:"logprobs"`
}

type ToolCallCompletionResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []ToolCallChoice `json:"choices"`
	Usage   OpenAIUsage      `json:"usage"`
}

func NewToolCallResponse(id string, created int64, model, text string, calls []*PendingToolCall, usage OpenAIUsage) ToolCallCompletionResponse {
	var content interface{}
	if text != "" {
		content = text
	}
	return ToolCallCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []ToolCallChoice{
			{
				Index: 0,
				Message: ToolCallMessage{
					Role:      "assistant",
					Content:   content,
					ToolCalls: ToolCallsFromPending(calls),
				},
				FinishReason: "tool_calls",
				Logprobs:     nil,
			},
		},
		Usage: usage,
	}
}

func ToolCallsFromPending(calls []*PendingToolCall) []ToolCall {
	toolCalls := make([]ToolCall, len(calls))
	for i, p := range calls {
		toolCalls[i] = ToolCall{
			ID:   p.ID,
			Type: "function",
			Function: ToolCallFunction{
				Name:      p.Name,
				Arguments: p.Arguments,
			},
		}
	}
	return toolCalls
}

type StreamChunkDelta map[string]interface{}

type StreamChunkChoice struct {
	Index        int              `json:"index"`
	Delta        StreamChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
	Logprobs     *string          `json:"logprobs"`
}

type StreamChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []StreamChunkChoice `json:"choices"`
	Usage   *OpenAIUsage        `json:"usage,omitempty"`
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
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    *string `json:"code"`
		Param   *string `json:"param"`
	} `json:"error"`
}

type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	OwnedBy string `json:"owned_by"`
	// ContextLength and MaxModelLen carry the same value under the two
	// field names OpenAI-compatible clients probe for when auto-detecting
	// a model's context window (see ADR-less issue #5).
	ContextLength int `json:"context_length,omitempty"`
	MaxModelLen   int `json:"max_model_len,omitempty"`
}
