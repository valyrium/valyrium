package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type ClaudeCliError struct {
	Message    string
	StatusCode int
}

func (e *ClaudeCliError) Error() string {
	return e.Message
}

type streamJsonLine struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	StopReason   *string  `json:"stop_reason"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage        *Usage   `json:"usage"`
	Message      *struct {
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Event *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type       string  `json:"type"`
			Text       string  `json:"text"`
			StopReason *string `json:"stop_reason"`
		} `json:"delta"`
		Usage *Usage `json:"usage"`
	} `json:"event"`
}

type RunClaudeOptions struct {
	ClaudeBin    string
	Prompt       string
	SystemPrompt string
	Model        string
	Effort       string
	TimeoutMs    int
	Signal       context.Context
	OnTextDelta  func(string)
}

func RunClaude(opts RunClaudeOptions) (*Completion, error) {
	ctx, cancel := context.WithTimeout(opts.Signal, time.Duration(opts.TimeoutMs)*time.Millisecond)
	defer cancel()

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--tools", "",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--system-prompt", opts.SystemPrompt,
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}

	cmd := exec.CommandContext(ctx, opts.ClaudeBin, args...)
	cmd.Stdin = strings.NewReader(opts.Prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &ClaudeCliError{
			Message:    fmt.Sprintf("failed to create stdout pipe: %v", err),
			StatusCode: 500,
		}
	}

	if _, err := cmd.StderrPipe(); err != nil {
		return nil, &ClaudeCliError{
			Message:    fmt.Sprintf("failed to create stderr pipe: %v", err),
			StatusCode: 500,
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, &ClaudeCliError{
			Message:    fmt.Sprintf("failed to spawn claude CLI: %v", err),
			StatusCode: 500,
		}
	}

	var (
		streamedText string
		resultText   *string
		model        string
		stopReason   *string
		costUsd      *float64
		usage        Usage
		sawResult    bool
		stderrTail   strings.Builder
		settledOnce  sync.Once
		settled      bool
		settleMutex  sync.Mutex
	)

	model = opts.Model
	if model == "" {
		model = "unknown"
	}

	finish := func(completion *Completion, err error) (*Completion, error) {
		settleMutex.Lock()
		defer settleMutex.Unlock()

		settledOnce.Do(func() {
			settled = true
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
			}
		})

		if err != nil {
			return nil, err
		}
		return completion, nil
	}

	go func() {
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			settleMutex.Lock()
			defer settleMutex.Unlock()
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		settleMutex.Lock()
		if settled {
			settleMutex.Unlock()
			break
		}
		settleMutex.Unlock()

		line := scanner.Text()
		if line == "" {
			continue
		}

		var parsed streamJsonLine
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}

		if parsed.Type == "stream_event" && parsed.Event != nil {
			if parsed.Event.Type == "content_block_delta" && parsed.Event.Delta != nil {
				if parsed.Event.Delta.Type == "text_delta" && parsed.Event.Delta.Text != "" {
					streamedText += parsed.Event.Delta.Text
					if opts.OnTextDelta != nil {
						opts.OnTextDelta(parsed.Event.Delta.Text)
					}
				}
			} else if parsed.Event.Type == "message_delta" && parsed.Event.Delta != nil && parsed.Event.Delta.StopReason != nil {
				stopReason = parsed.Event.Delta.StopReason
			}
			continue
		}

		if parsed.Type == "assistant" && parsed.Message != nil && parsed.Message.Model != "" {
			model = parsed.Message.Model
			continue
		}

		if parsed.Type == "result" {
			sawResult = true
			if parsed.IsError {
				detail := parsed.Result
				if detail == "" {
					detail = parsed.Subtype
				}
				if detail == "" {
					detail = "unknown error"
				}
				_ = cmd.Wait()
				return finish(nil, &ClaudeCliError{
					Message:    fmt.Sprintf("claude CLI reported an error: %s", detail),
					StatusCode: 502,
				})
			}
			if parsed.Result != "" {
				resultText = &parsed.Result
			}
			if parsed.StopReason != nil {
				stopReason = parsed.StopReason
			}
			if parsed.TotalCostUSD != nil {
				costUsd = parsed.TotalCostUSD
			}
			if parsed.Usage != nil {
				usage = *parsed.Usage
			}
		}
	}

	_ = cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return finish(nil, &ClaudeCliError{
			Message:    fmt.Sprintf("claude CLI timed out after %dms", opts.TimeoutMs),
			StatusCode: 504,
		})
	}

	if !sawResult {
		detail := fmt.Sprintf("exit code %d", cmd.ProcessState.ExitCode())
		if stderrTail.Len() > 0 {
			detail = stderrTail.String()
		}
		return finish(nil, &ClaudeCliError{
			Message:    fmt.Sprintf("claude CLI exited without a result: %s", detail),
			StatusCode: 502,
		})
	}

	text := streamedText
	if resultText != nil {
		text = *resultText
	}

	completion := &Completion{
		Text:       text,
		Model:      model,
		StopReason: stopReason,
		Usage:      usage,
		CostUSD:    costUsd,
	}

	return finish(completion, nil)
}

type SessionRunOptions struct {
	ClaudeBin     string
	Prompt        string
	SystemPrompt  string
	Model         string
	Effort        string
	MCPURL        string
	ToolTimeoutMS int
}

// mcpRelayServerName is the MCP server name the gateway registers itself
// under in the spawned CLI's --mcp-config. Claude Code exposes MCP tools to
// the model under the qualified name "mcp__<server>__<tool>" to avoid
// collisions with its own built-in tools, so any tool_use the model proposes
// arrives prefixed this way and must be stripped before it is handed back to
// an OpenAI-facing client, which only knows the bare name it declared.
const mcpRelayServerName = "relay"

func stripMCPToolPrefix(name string) string {
	prefix := "mcp__" + mcpRelayServerName + "__"
	return strings.TrimPrefix(name, prefix)
}

// StartClaudeSession spawns a tool-capable CLI process pointed at the
// gateway's own MCP endpoint for this session. The process is owned by the
// session table, not by any HTTP request: it survives the first response
// and is killed only when the session is reaped. Stream output is relayed
// onto sess.Events by a reader goroutine.
func StartClaudeSession(sm *SessionManager, sess *Session, opts SessionRunOptions) error {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--tools", "",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--system-prompt", opts.SystemPrompt,
		"--strict-mcp-config",
		"--mcp-config", fmt.Sprintf(`{"mcpServers":{%q:{"type":"http","url":"%s"}}}`, mcpRelayServerName, opts.MCPURL),
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}

	cmd := exec.Command(opts.ClaudeBin, args...)
	cmd.Stdin = strings.NewReader(opts.Prompt)
	// The CLI's own MCP timeout must not fire before the gateway's tool
	// timeout does, or it would kill a legitimately parked call.
	mcpTimeout := opts.ToolTimeoutMS + 60000
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", mcpTimeout),
		fmt.Sprintf("MCP_TIMEOUT=%d", mcpTimeout),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &ClaudeCliError{
			Message:    fmt.Sprintf("failed to create stdout pipe: %v", err),
			StatusCode: 500,
		}
	}

	if err := cmd.Start(); err != nil {
		return &ClaudeCliError{
			Message:    fmt.Sprintf("failed to spawn claude CLI: %v", err),
			StatusCode: 500,
		}
	}

	sess.Mu.Lock()
	sess.Cmd = cmd
	sess.Mu.Unlock()

	go relaySessionStream(sm, sess, stdout, cmd)
	return nil
}

func relaySessionStream(sm *SessionManager, sess *Session, stdout io.Reader, cmd *exec.Cmd) {
	defer close(sess.Events)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawResult := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var parsed streamJsonLine
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}

		switch parsed.Type {
		case "stream_event":
			if parsed.Event == nil {
				continue
			}
			if parsed.Event.Type == "content_block_delta" && parsed.Event.Delta != nil &&
				parsed.Event.Delta.Type == "text_delta" && parsed.Event.Delta.Text != "" {
				sess.Events <- SessionEvent{Type: "text", Text: parsed.Event.Delta.Text}
			} else if parsed.Event.Type == "message_delta" {
				ev := SessionEvent{Type: "stop", Usage: parsed.Event.Usage}
				if parsed.Event.Delta != nil {
					ev.StopReason = parsed.Event.Delta.StopReason
				}
				sess.Events <- ev
			}
		case "assistant":
			if parsed.Message == nil {
				continue
			}
			var uses []ToolUseBlock
			for _, block := range parsed.Message.Content {
				if block.Type == "tool_use" {
					uses = append(uses, ToolUseBlock{ID: block.ID, Name: stripMCPToolPrefix(block.Name), Input: block.Input})
				}
			}
			ev := SessionEvent{Type: "tool_calls", Model: parsed.Message.Model}
			if len(uses) > 0 {
				// Register at announcement time so the CLI's MCP
				// tools/call can never race ahead of the pending table.
				ev.Calls = sm.RegisterAnnouncedCalls(sess, uses)
			}
			if ev.Model != "" || len(ev.Calls) > 0 {
				sess.Events <- ev
			}
		case "result":
			sawResult = true
			ev := SessionEvent{
				Type:       "run_end",
				StopReason: parsed.StopReason,
				CostUSD:    parsed.TotalCostUSD,
				Usage:      parsed.Usage,
			}
			if parsed.Result != "" {
				resultText := parsed.Result
				ev.ResultText = &resultText
			}
			if parsed.IsError {
				detail := parsed.Result
				if detail == "" {
					detail = parsed.Subtype
				}
				if detail == "" {
					detail = "unknown error"
				}
				ev.Err = &ClaudeCliError{
					Message:    fmt.Sprintf("claude CLI reported an error: %s", detail),
					StatusCode: 502,
				}
			}
			sess.Events <- ev
		}
	}

	_ = cmd.Wait()

	if !sawResult {
		sess.Events <- SessionEvent{
			Type: "run_end",
			Err: &ClaudeCliError{
				Message:    "claude CLI exited without a result",
				StatusCode: 502,
			},
		}
	}
}
