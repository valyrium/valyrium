package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	IsError  bool   `json:"is_error"`
	Result   string `json:"result"`
	StopReason *string `json:"stop_reason"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage    *Usage `json:"usage"`
	Message  *struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Event *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type       string `json:"type"`
			Text       string `json:"text"`
			StopReason *string `json:"stop_reason"`
		} `json:"delta"`
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
				cmd.Process.Kill()
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
				cmd.Process.Kill()
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
				cmd.Wait()
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

	err = cmd.Wait()
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
