package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain doubles as the stub claude CLI: when CLAUDE_STUB_MODE is set,
// the test binary re-executed by the gateway behaves like a tool-calling
// CLI that speaks stream-json on stdout and MCP over HTTP back to the
// gateway's /mcp/{sessionId} endpoint.
func TestMain(m *testing.M) {
	if mode := os.Getenv("CLAUDE_STUB_MODE"); mode != "" {
		os.Exit(runClaudeStub(mode))
	}
	os.Exit(m.Run())
}

func runClaudeStub(mode string) int {
	// The resume stub drives the stateless path: it speaks no MCP, it just
	// records how it was invoked (see resume_test.go).
	if mode == "resume" {
		return runResumeStub()
	}

	var mcpConfig string
	for i, a := range os.Args {
		if a == "--mcp-config" && i+1 < len(os.Args) {
			mcpConfig = os.Args[i+1]
		}
	}
	_, _ = io.Copy(io.Discard, os.Stdin)

	fail := func(msg string) int {
		line, _ := json.Marshal(map[string]interface{}{"type": "result", "is_error": true, "result": msg})
		fmt.Println(string(line))
		return 1
	}

	if mode == "reasoning" {
		// A plain (non-tool-calling) turn spawned via RunClaude, which never
		// sets --mcp-config: emits a thinking delta ahead of its final text
		// answer, exercising the reasoning_content relay.
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"Let me think about this."}}}`)
		stubEmitFinal("The answer is 42")
		return 0
	}

	var cfg struct {
		McpServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpConfig), &cfg); err != nil {
		return fail("stub: bad --mcp-config: " + err.Error())
	}
	url := cfg.McpServers["relay"].URL
	if url == "" {
		return fail("stub: no relay url in --mcp-config")
	}

	client := &http.Client{} // no timeout: parked tools/call must wait

	if _, err := stubRPC(client, url, 1, "initialize", map[string]interface{}{}); err != nil {
		return fail("stub: initialize failed: " + err.Error())
	}
	listResp, err := stubRPC(client, url, 2, "tools/list", map[string]interface{}{})
	if err != nil {
		return fail("stub: tools/list failed: " + err.Error())
	}

	switch mode {
	case "relay":
		if !stubToolsListHas(listResp, "get_weather") {
			return fail("stub: tools/list missing get_weather")
		}
		fmt.Println(`{"type":"assistant","message":{"model":"claude-stub","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Nairobi"}}]}}`)
		fmt.Println(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)

		callResp, err := stubRPC(client, url, 3, "tools/call", map[string]interface{}{
			"name":      "get_weather",
			"arguments": map[string]interface{}{"city": "Nairobi"},
		})
		if err != nil {
			return fail("stub: tools/call failed: " + err.Error())
		}
		stubEmitFinal("The weather is " + stubRPCResultText(callResp))
	case "relay_prefixed":
		// The real claude CLI exposes MCP tools to the model under the
		// qualified name "mcp__<server>__<tool>" (here "mcp__relay__..."),
		// but still issues the actual tools/call JSON-RPC request with the
		// bare name from tools/list. The gateway must strip the prefix
		// before handing the tool name back to an OpenAI-facing client.
		if !stubToolsListHas(listResp, "get_weather") {
			return fail("stub: tools/list missing get_weather")
		}
		fmt.Println(`{"type":"assistant","message":{"model":"claude-stub","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__relay__get_weather","input":{"city":"Nairobi"}}]}}`)
		fmt.Println(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)

		callResp, err := stubRPC(client, url, 3, "tools/call", map[string]interface{}{
			"name":      "get_weather",
			"arguments": map[string]interface{}{"city": "Nairobi"},
		})
		if err != nil {
			return fail("stub: tools/call failed: " + err.Error())
		}
		stubEmitFinal("The weather is " + stubRPCResultText(callResp))
	case "sequential":
		// Announce both calls, then dispatch them strictly sequentially:
		// tools/call #2 is only issued after #1's MCP response arrives.
		// This is the dispatch pattern that deadlocks any design that
		// waits for all MCP calls before sending the HTTP response.
		fmt.Println(`{"type":"assistant","message":{"model":"claude-stub","content":[{"type":"tool_use","id":"toolu_1","name":"tool_one","input":{"n":1}},{"type":"tool_use","id":"toolu_2","name":"tool_two","input":{"n":2}}]}}`)
		fmt.Println(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)

		r1, err := stubRPC(client, url, 3, "tools/call", map[string]interface{}{
			"name":      "tool_one",
			"arguments": map[string]interface{}{"n": 1},
		})
		if err != nil {
			return fail("stub: tools/call tool_one failed: " + err.Error())
		}
		r2, err := stubRPC(client, url, 4, "tools/call", map[string]interface{}{
			"name":      "tool_two",
			"arguments": map[string]interface{}{"n": 2},
		})
		if err != nil {
			return fail("stub: tools/call tool_two failed: " + err.Error())
		}
		stubEmitFinal("got:" + stubRPCResultText(r1) + "," + stubRPCResultText(r2))
	default:
		return fail("stub: unknown mode " + mode)
	}
	return 0
}

func stubRPC(client *http.Client, url string, id int, method string, params interface{}) (map[string]interface{}, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if e, ok := out["error"]; ok && e != nil {
		return nil, fmt.Errorf("rpc error: %v", e)
	}
	return out, nil
}

func stubRPCResultText(out map[string]interface{}) string {
	result, _ := out["result"].(map[string]interface{})
	content, _ := result["content"].([]interface{})
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]interface{})
	text, _ := first["text"].(string)
	return text
}

func stubToolsListHas(out map[string]interface{}, name string) bool {
	result, _ := out["result"].(map[string]interface{})
	tools, _ := result["tools"].([]interface{})
	for _, tool := range tools {
		m, _ := tool.(map[string]interface{})
		if m["name"] == name {
			return true
		}
	}
	return false
}

func stubEmitFinal(answer string) {
	delta, _ := json.Marshal(map[string]interface{}{
		"type": "stream_event",
		"event": map[string]interface{}{
			"type":  "content_block_delta",
			"delta": map[string]interface{}{"type": "text_delta", "text": answer},
		},
	})
	fmt.Println(string(delta))

	res, _ := json.Marshal(map[string]interface{}{
		"type":           "result",
		"result":         answer,
		"stop_reason":    "end_turn",
		"total_cost_usd": 0.002,
		"usage": map[string]int{
			"input_tokens":                12,
			"output_tokens":               6,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		},
	})
	fmt.Println(string(res))
}

// --- test-side helpers -----------------------------------------------

func newRelayServer(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cfg.ClaudeBin = exe
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "sonnet"
	}
	if cfg.Models == nil {
		cfg.Models = []string{"sonnet"}
	}
	if cfg.TimeoutMS == 0 {
		cfg.TimeoutMS = 8000
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 4
	}
	if cfg.UsageDB == "" {
		cfg.UsageDB = "off"
	}

	server := NewServer(cfg)
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	// Runs before ts.Close (LIFO): unblocks any still-parked MCP handler
	// so the httptest server can drain its outstanding requests.
	t.Cleanup(server.sessions.Close)
	server.SetMCPBaseURL(ts.URL)
	return server, ts
}

type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string      `json:"role"`
			Content          interface{} `json:"content"`
			ReasoningContent string      `json:"reasoning_content"`
			ToolCalls        []ToolCall  `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage OpenAIUsage `json:"usage"`
}

func postChat(t *testing.T, baseURL string, body map[string]interface{}) (int, *chatResponse) {
	t.Helper()

	payload, _ := json.Marshal(body)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal response (status %d): %v\nbody: %s", resp.StatusCode, err, raw)
	}
	return resp.StatusCode, &parsed
}

func weatherTools() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "get_weather",
				"description": "Get current weather for a city",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"city": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
	}
}

func toolCallsAsMessages(calls []ToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, len(calls))
	for i, c := range calls {
		out[i] = map[string]interface{}{
			"id":   c.ID,
			"type": c.Type,
			"function": map[string]interface{}{
				"name":      c.Function.Name,
				"arguments": c.Function.Arguments,
			},
		}
	}
	return out
}

// --- tests -------------------------------------------------------------

// TestMCPRelay proves the full two-turn round trip: tools register over
// MCP, the announced tool call pauses the session and surfaces as
// finish_reason "tool_calls", and the client's tool result resumes the
// parked CLI to a final answer.
func TestMCPRelay(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "relay")
	_, ts := newRelayServer(t, Config{})

	userMsg := map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}

	status, first := postChat(t, ts.URL, map[string]interface{}{
		"model":    "sonnet",
		"messages": []interface{}{userMsg},
		"tools":    weatherTools(),
	})
	if status != 200 {
		t.Fatalf("first turn: expected 200, got %d", status)
	}
	if len(first.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(first.Choices))
	}
	choice := first.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", choice.FinishReason)
	}
	if choice.Message.Content != nil {
		t.Errorf("expected null content alongside tool_calls, got %v", choice.Message.Content)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(choice.Message.ToolCalls))
	}
	call := choice.Message.ToolCalls[0]
	if !strings.HasPrefix(call.ID, "call_") {
		t.Errorf("tool_call id should start with call_, got %q", call.ID)
	}
	if call.Type != "function" || call.Function.Name != "get_weather" {
		t.Errorf("unexpected tool call: %+v", call)
	}
	if !strings.Contains(call.Function.Arguments, "Nairobi") {
		t.Errorf("arguments should carry the announced input, got %q", call.Function.Arguments)
	}

	status, second := postChat(t, ts.URL, map[string]interface{}{
		"model": "sonnet",
		"messages": []interface{}{
			userMsg,
			map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": toolCallsAsMessages(choice.Message.ToolCalls)},
			map[string]interface{}{"role": "tool", "tool_call_id": call.ID, "content": "sunny, 24C"},
		},
		"tools": weatherTools(),
	})
	if status != 200 {
		t.Fatalf("second turn: expected 200, got %d", status)
	}
	if second.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason stop, got %q", second.Choices[0].FinishReason)
	}
	if got := second.Choices[0].Message.Content; got != "The weather is sunny, 24C" {
		t.Errorf("final answer should embed the tool result, got %v", got)
	}
}

// TestMCPRelayStripsToolNamePrefix pins the fix for a real bug found by
// running an actual OpenAI-tool-calling client (opencode) through the
// gateway against the real claude CLI: Claude Code exposes MCP tools to the
// model as "mcp__relay__<tool>", and the gateway must strip that prefix
// before returning tool_calls, or the client won't recognize its own tool.
func TestMCPRelayStripsToolNamePrefix(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "relay_prefixed")
	_, ts := newRelayServer(t, Config{})

	userMsg := map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}

	status, first := postChat(t, ts.URL, map[string]interface{}{
		"model":    "sonnet",
		"messages": []interface{}{userMsg},
		"tools":    weatherTools(),
	})
	if status != 200 {
		t.Fatalf("first turn: expected 200, got %d", status)
	}
	if len(first.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(first.Choices[0].Message.ToolCalls))
	}
	call := first.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "get_weather" {
		t.Errorf("expected the mcp__relay__ prefix stripped, got function.name %q", call.Function.Name)
	}

	status, second := postChat(t, ts.URL, map[string]interface{}{
		"model": "sonnet",
		"messages": []interface{}{
			userMsg,
			map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": toolCallsAsMessages(first.Choices[0].Message.ToolCalls)},
			map[string]interface{}{"role": "tool", "tool_call_id": call.ID, "content": "sunny, 24C"},
		},
		"tools": weatherTools(),
	})
	if status != 200 {
		t.Fatalf("second turn: expected 200, got %d", status)
	}
	if got := second.Choices[0].Message.Content; got != "The weather is sunny, 24C" {
		t.Errorf("final answer should embed the tool result, got %v", got)
	}
}

// TestShutdownReapsParkedSessions pins issue #8: a parked tool-calling
// session (its CLI process, MCP goroutine, and Events channel) must not
// outlive a graceful Server.Shutdown.
func TestShutdownReapsParkedSessions(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "relay")
	server, ts := newRelayServer(t, Config{})

	userMsg := map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}
	status, first := postChat(t, ts.URL, map[string]interface{}{
		"model":    "sonnet",
		"messages": []interface{}{userMsg},
		"tools":    weatherTools(),
	})
	if status != 200 {
		t.Fatalf("first turn: expected 200, got %d", status)
	}
	if first.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", first.Choices[0].FinishReason)
	}
	if got := server.sessions.GetSessionCount(); got != 1 {
		t.Fatalf("expected 1 parked session before shutdown, got %d", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := server.sessions.GetSessionCount(); got != 0 {
		t.Errorf("expected Shutdown to reap parked sessions, got %d still live", got)
	}
}

// TestStreamingToolTurnLogsUsage covers a path no prior test exercised: a
// STREAMING tool-calling turn (what a real OpenAI-tool-calling client like
// opencode actually sends). Found via a real opencode dogfood run: the
// structured stderr log for streaming tool turns always reported
// prompt_tokens:0, completion_tokens:0 because streamToolTurn never wrote
// usage back to the request log. Also re-confirms the tool-name-prefix
// strip (§ relay_prefixed) survives the streaming path.
func TestStreamingToolTurnLogsUsage(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "relay_prefixed")
	_, ts := newRelayServer(t, Config{})

	payload, _ := json.Marshal(map[string]interface{}{
		"model":    "sonnet",
		"stream":   true,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}},
		"tools":    weatherTools(),
	})

	var sseBody string
	logLine := captureStderr(t, func() {
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST /v1/chat/completions: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		sseBody = string(raw)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, sseBody)
		}
	})

	if !strings.Contains(sseBody, `"name":"get_weather"`) {
		t.Errorf("expected bare tool name get_weather in SSE tool_calls chunk, got: %s", sseBody)
	}
	if strings.Contains(sseBody, "mcp__relay__") {
		t.Errorf("mcp__relay__ prefix leaked into the SSE response: %s", sseBody)
	}
	if !strings.Contains(sseBody, `"finish_reason":"tool_calls"`) {
		t.Errorf("expected finish_reason tool_calls in SSE stream, got: %s", sseBody)
	}

	var entry struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	if err := json.Unmarshal([]byte(logLine), &entry); err != nil {
		t.Fatalf("stderr log line was not valid JSON: %v\nline: %s", err, logLine)
	}
	if entry.PromptTokens != 7 || entry.CompletionTokens != 3 {
		t.Errorf("expected prompt_tokens=7 completion_tokens=3 (from the stub's message_delta usage) in the request log, got %+v", entry)
	}
}

// TestSequentialDispatchNoDeadlock proves finalization is driven by the
// stream announcement, never by MCP traffic: the stub issues its second
// tools/call only after the first one resolves, which deadlocks any design
// that holds the HTTP response until all MCP calls arrive (ADR 0001 §4).
func TestSequentialDispatchNoDeadlock(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "sequential")
	_, ts := newRelayServer(t, Config{})

	tools := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "tool_one", "description": "first", "parameters": map[string]interface{}{"type": "object"}}},
		{"type": "function", "function": map[string]interface{}{"name": "tool_two", "description": "second", "parameters": map[string]interface{}{"type": "object"}}},
	}
	userMsg := map[string]interface{}{"role": "user", "content": "Run both tools"}

	status, first := postChat(t, ts.URL, map[string]interface{}{
		"model":    "sonnet",
		"messages": []interface{}{userMsg},
		"tools":    tools,
	})
	if status != 200 {
		t.Fatalf("first turn: expected 200, got %d", status)
	}
	calls := first.Choices[0].Message.ToolCalls
	if first.Choices[0].FinishReason != "tool_calls" || len(calls) != 2 {
		t.Fatalf("expected tool_calls with 2 calls, got %q with %d", first.Choices[0].FinishReason, len(calls))
	}
	if calls[0].Function.Name != "tool_one" || calls[1].Function.Name != "tool_two" {
		t.Fatalf("tool calls out of announcement order: %+v", calls)
	}

	status, second := postChat(t, ts.URL, map[string]interface{}{
		"model": "sonnet",
		"messages": []interface{}{
			userMsg,
			map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": toolCallsAsMessages(calls)},
			map[string]interface{}{"role": "tool", "tool_call_id": calls[0].ID, "content": "one"},
			map[string]interface{}{"role": "tool", "tool_call_id": calls[1].ID, "content": "two"},
		},
		"tools": tools,
	})
	if status != 200 {
		t.Fatalf("second turn: expected 200, got %d", status)
	}
	if got := second.Choices[0].Message.Content; got != "got:one,two" {
		t.Errorf("expected sequential dispatch to resolve both calls, got %v", got)
	}
}

// TestParallelSessionsNoCrossWire runs two byte-identical conversations in
// parallel and proves tool_call_id correlation routes each continuation to
// its own session (the failure mode that killed prefix-hash correlation).
func TestParallelSessionsNoCrossWire(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "relay")
	_, ts := newRelayServer(t, Config{})

	userMsg := map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}
	firstBody := map[string]interface{}{
		"model":    "sonnet",
		"messages": []interface{}{userMsg},
		"tools":    weatherTools(),
	}

	firsts := make([]*chatResponse, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, resp := postChat(t, ts.URL, firstBody)
			if status != 200 {
				t.Errorf("session %d first turn: expected 200, got %d", i, status)
				return
			}
			firsts[i] = resp
		}(i)
	}
	wg.Wait()
	if t.Failed() {
		t.Fatal("first turns failed")
	}

	idA := firsts[0].Choices[0].Message.ToolCalls[0].ID
	idB := firsts[1].Choices[0].Message.ToolCalls[0].ID
	if idA == idB {
		t.Fatalf("tool_call_ids must be globally unique, both were %q", idA)
	}

	results := map[string]string{idA: "sunny-A", idB: "rainy-B"}
	finals := make(map[string]interface{}, 2)
	for i, id := range []string{idA, idB} {
		status, resp := postChat(t, ts.URL, map[string]interface{}{
			"model": "sonnet",
			"messages": []interface{}{
				userMsg,
				map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": toolCallsAsMessages(firsts[i].Choices[0].Message.ToolCalls)},
				map[string]interface{}{"role": "tool", "tool_call_id": id, "content": results[id]},
			},
			"tools": weatherTools(),
		})
		if status != 200 {
			t.Fatalf("continuation for %s: expected 200, got %d", id, status)
		}
		finals[id] = resp.Choices[0].Message.Content
	}

	if finals[idA] != "The weather is sunny-A" {
		t.Errorf("session A cross-wired: got %v", finals[idA])
	}
	if finals[idB] != "The weather is rainy-B" {
		t.Errorf("session B cross-wired: got %v", finals[idB])
	}
}

// TestSessionLifecycle covers session accounting: sessions appear while
// parked, the MaxSessions cap yields 429, run end reaps, and the sweeper
// reaps a parked session whose tool result never arrives.
func TestSessionLifecycle(t *testing.T) {
	t.Setenv("CLAUDE_STUB_MODE", "relay")

	t.Run("park, cap, resume, reap", func(t *testing.T) {
		server, ts := newRelayServer(t, Config{MaxSessions: 1})

		if n := server.sessions.GetSessionCount(); n != 0 {
			t.Fatalf("expected 0 sessions initially, got %d", n)
		}

		userMsg := map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}
		status, first := postChat(t, ts.URL, map[string]interface{}{
			"model":    "sonnet",
			"messages": []interface{}{userMsg},
			"tools":    weatherTools(),
		})
		if status != 200 || first.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("first turn: status %d, finish %q", status, first.Choices[0].FinishReason)
		}
		if n := server.sessions.GetSessionCount(); n != 1 {
			t.Fatalf("expected 1 live session while parked, got %d", n)
		}

		// The parked session holds the only slot under MaxSessions=1.
		status, _ = postChat(t, ts.URL, map[string]interface{}{
			"model":    "sonnet",
			"messages": []interface{}{userMsg},
			"tools":    weatherTools(),
		})
		if status != 429 {
			t.Fatalf("expected 429 at session cap, got %d", status)
		}

		call := first.Choices[0].Message.ToolCalls[0]
		status, second := postChat(t, ts.URL, map[string]interface{}{
			"model": "sonnet",
			"messages": []interface{}{
				userMsg,
				map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": toolCallsAsMessages(first.Choices[0].Message.ToolCalls)},
				map[string]interface{}{"role": "tool", "tool_call_id": call.ID, "content": "cool, 18C"},
			},
			"tools": weatherTools(),
		})
		if status != 200 || second.Choices[0].Message.Content != "The weather is cool, 18C" {
			t.Fatalf("continuation: status %d, content %v", status, second.Choices[0].Message.Content)
		}
		if n := server.sessions.GetSessionCount(); n != 0 {
			t.Fatalf("expected session reaped after run end, got %d", n)
		}
	})

	t.Run("tool timeout sweeper reaps abandoned session", func(t *testing.T) {
		server, ts := newRelayServer(t, Config{ToolTimeoutMS: 300})

		status, first := postChat(t, ts.URL, map[string]interface{}{
			"model":    "sonnet",
			"messages": []interface{}{map[string]interface{}{"role": "user", "content": "What's the weather in Nairobi?"}},
			"tools":    weatherTools(),
		})
		if status != 200 || first.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("first turn: status %d, finish %q", status, first.Choices[0].FinishReason)
		}
		if n := server.sessions.GetSessionCount(); n != 1 {
			t.Fatalf("expected 1 live session while parked, got %d", n)
		}

		// Never send the tool result; the sweeper must reap the session.
		deadline := time.Now().Add(3 * time.Second)
		for server.sessions.GetSessionCount() != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("session not reaped after tool timeout; count %d", server.sessions.GetSessionCount())
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
}

// TestColdHistoryFlattening proves tool exchanges the gateway has no live
// session for are serialized into the transcript (ADR 0001 §3) instead of
// being rejected.
func TestColdHistoryFlattening(t *testing.T) {
	messages := []OpenAIMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "What's the weather in Nairobi?"},
		{Role: "assistant", Content: nil, ToolCalls: []ToolCall{
			{
				ID:   "call_abc123",
				Type: "function",
				Function: ToolCallFunction{
					Name:      "get_weather",
					Arguments: `{"city":"Nairobi"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_abc123", Content: `{"temp_c":24}`},
		{Role: "user", Content: "And tomorrow?"},
	}

	system, prompt, err := BuildPrompt(messages)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(prompt, `[assistant called tool get_weather (call_abc123)]: {"city":"Nairobi"}`) {
		t.Errorf("prompt missing tool-call marker:\n%s", prompt)
	}
	if !strings.Contains(prompt, `[tool get_weather (call_abc123) returned]: {"temp_c":24}`) {
		t.Errorf("prompt missing tool-result marker:\n%s", prompt)
	}
	if !strings.HasSuffix(prompt, "[user]: And tomorrow?") {
		t.Errorf("prompt should end with the final user turn:\n%s", prompt)
	}
	if !strings.Contains(system, toolTranscriptInstruction) {
		t.Errorf("system prompt missing tool-marker instruction:\n%s", system)
	}

	t.Run("trailing tool result is a valid last turn", func(t *testing.T) {
		_, prompt, err := BuildPrompt(messages[:4])
		if err != nil {
			t.Fatalf("BuildPrompt: %v", err)
		}
		if !strings.HasSuffix(prompt, `[tool get_weather (call_abc123) returned]: {"temp_c":24}`) {
			t.Errorf("prompt should end with the tool result:\n%s", prompt)
		}
	})

	t.Run("tool message without tool_call_id still fails", func(t *testing.T) {
		if _, _, err := BuildPrompt([]OpenAIMessage{{Role: "tool", Content: "{}"}}); err == nil {
			t.Error("expected an error for a tool message with no tool_call_id")
		}
	})
}
