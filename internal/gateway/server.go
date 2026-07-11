package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Port          int
	Host          string
	APIKey        string
	DefaultModel  string
	Models        []string
	ClaudeBin     string
	TimeoutMS     int
	Concurrency   int
	MaxSessions   int // cap on live tool-calling CLI sessions (default 16)
	ToolTimeoutMS int // max wait for a client tool result (default 120000)
	SessionIdleMS int // idle session reap threshold (default 600000)
}

type Semaphore struct {
	available int
	waiters   []chan struct{}
	mu        sync.Mutex
}

func NewSemaphore(size int) *Semaphore {
	if size < 1 {
		size = 1
	}
	return &Semaphore{available: size}
}

func (s *Semaphore) Acquire(ctx context.Context) error {
	s.mu.Lock()
	if s.available > 0 {
		s.available--
		s.mu.Unlock()
		return nil
	}

	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Semaphore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.waiters) > 0 {
		ch := s.waiters[0]
		s.waiters = s.waiters[1:]
		select {
		case ch <- struct{}{}:
		default:
		}
	} else {
		s.available++
	}
}

type Server struct {
	config     Config
	slots      *Semaphore
	metrics    *Metrics
	sessions   *SessionManager
	mcpBaseURL string
	listeners  []net.Listener
	doneCh     chan struct{}
}

func NewServer(config Config) *Server {
	if config.MaxSessions <= 0 {
		config.MaxSessions = 16
	}
	if config.ToolTimeoutMS <= 0 {
		config.ToolTimeoutMS = 120000
	}
	if config.SessionIdleMS <= 0 {
		config.SessionIdleMS = 600000
	}
	return &Server{
		config:     config,
		slots:      NewSemaphore(config.Concurrency),
		metrics:    NewMetrics(),
		sessions:   NewSessionManager(config.MaxSessions, config.ToolTimeoutMS, config.SessionIdleMS),
		mcpBaseURL: fmt.Sprintf("http://127.0.0.1:%d", config.Port),
		doneCh:     make(chan struct{}),
	}
}

// SetMCPBaseURL overrides the base URL the spawned CLI uses to reach the
// gateway's own MCP endpoint (used when the real listen address is not
// known from config, e.g. under httptest).
func (s *Server) SetMCPBaseURL(baseURL string) {
	s.mcpBaseURL = strings.TrimSuffix(baseURL, "/")
}

func (s *Server) isAuthorized(r *http.Request) bool {
	if s.config.APIKey == "" {
		return true
	}

	bearer := r.Header.Get("Authorization")
	expectedBearer := fmt.Sprintf("Bearer %s", s.config.APIKey)
	if subtle.ConstantTimeCompare([]byte(bearer), []byte(expectedBearer)) == 1 {
		return true
	}

	apiKey := r.Header.Get("x-api-key")
	if subtle.ConstantTimeCompare([]byte(apiKey), []byte(s.config.APIKey)) == 1 {
		return true
	}

	return false
}

func (s *Server) sendJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (s *Server) sendError(w http.ResponseWriter, status int, message string, errType string) {
	resp := ErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = errType
	resp.Error.Code = nil
	resp.Error.Param = nil
	s.sendJSON(w, status, resp)
}

func (s *Server) resolveModel(requested string) string {
	if requested == "" {
		return s.config.DefaultModel
	}

	normalized := strings.ToLower(strings.TrimSpace(requested))

	if strings.HasPrefix(normalized, "claude") {
		return strings.TrimSpace(requested)
	}

	aliases := make(map[string]bool)
	aliases["sonnet"] = true
	aliases["opus"] = true
	aliases["haiku"] = true
	aliases["fable"] = true
	for _, m := range s.config.Models {
		aliases[strings.ToLower(m)] = true
	}

	if aliases[normalized] {
		return strings.TrimSpace(requested)
	}

	return s.config.DefaultModel
}

func (s *Server) readBody(r *http.Request, maxSize int) (string, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, int64(maxSize))
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("request body too large")
	}
	return string(data), nil
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	statusCode := 200
	model := s.config.DefaultModel
	promptTokens := 0
	completionTokens := 0

	defer func() {
		duration := time.Since(startTime)
		s.metrics.RecordRequest(r.Method, r.URL.Path, statusCode, duration)

		logEntry := map[string]interface{}{
			"method":            r.Method,
			"path":              r.URL.Path,
			"model":             model,
			"status":            statusCode,
			"duration_ms":       duration.Milliseconds(),
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
		}
		data, _ := json.Marshal(logEntry)
		fmt.Fprintf(os.Stderr, "%s\n", string(data))
	}()

	body, err := s.readBody(r, 32*1024*1024)
	if err != nil {
		statusCode = 400
		s.sendError(w, 400, err.Error(), "invalid_request_error")
		return
	}

	var req ChatCompletionRequest
	if err = json.Unmarshal([]byte(body), &req); err != nil {
		statusCode = 400
		s.sendError(w, 400, "request body must be valid JSON", "invalid_request_error")
		return
	}

	model = s.resolveModel(req.Model)
	effort := ""
	if req.ReasoningEffort != "" {
		efforts := map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
		if efforts[req.ReasoningEffort] {
			effort = req.ReasoningEffort
		}
	}

	id := newCompletionID()
	created := time.Now().Unix()

	// MCP relay routing (ADR 0001): a request whose trailing tool messages
	// correlate to a live session resumes it; a request carrying tools
	// starts a session. Everything else takes the stateless path, with
	// cold tool history flattened into the transcript by BuildPrompt.
	contSess, toolResults := s.findContinuationSession(req.Messages)
	wantsTools := len(req.Tools) > 0 && !toolChoiceIsNone(req.ToolChoice)

	if contSess != nil || wantsTools {
		if err := s.slots.Acquire(r.Context()); err != nil {
			statusCode = 503
			s.sendError(w, 503, "service temporarily unavailable", "api_error")
			return
		}
		defer s.slots.Release()
		statusCode = s.handleToolTurn(w, r, req, contSess, toolResults, id, created, model, effort, &promptTokens, &completionTokens)
		return
	}

	systemPrompt, prompt, err := BuildPrompt(req.Messages)
	if err != nil {
		statusCode = 400
		s.sendError(w, 400, err.Error(), "invalid_request_error")
		return
	}

	if err = s.slots.Acquire(r.Context()); err != nil {
		statusCode = 503
		s.sendError(w, 503, "service temporarily unavailable", "api_error")
		return
	}
	defer s.slots.Release()

	if req.Stream {
		s.handleStreamingResponse(w, r, id, created, model, effort, systemPrompt, prompt, &promptTokens, &completionTokens)
		return
	}

	completion, err := RunClaude(RunClaudeOptions{
		ClaudeBin:    s.config.ClaudeBin,
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
		Model:        model,
		Effort:       effort,
		TimeoutMs:    s.config.TimeoutMS,
		Signal:       r.Context(),
	})

	if err != nil {
		if cliErr, ok := err.(*ClaudeCliError); ok {
			statusCode = cliErr.StatusCode
			s.sendError(w, cliErr.StatusCode, cliErr.Message, "api_error")
		} else {
			statusCode = 500
			s.sendError(w, 500, err.Error(), "api_error")
		}
		return
	}

	promptTokens = completion.Usage.InputTokens + completion.Usage.CacheReadInputTokens + completion.Usage.CacheCreationInputTokens
	completionTokens = completion.Usage.OutputTokens
	s.sendJSON(w, 200, CompletionResponseWithCost(id, created, *completion))
}

func (s *Server) handleStreamingResponse(w http.ResponseWriter, r *http.Request, id string, created int64, model, effort, systemPrompt, prompt string, promptTokens, completionTokens *int) {
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")

	w.WriteHeader(200)

	write := func(data interface{}) {
		if data != nil {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "data: %s\n\n", string(b))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}

	write(NewStreamChunk(id, created, model, StreamChunkDelta{"role": "assistant", "content": ""}, nil, nil))

	completion, err := RunClaude(RunClaudeOptions{
		ClaudeBin:    s.config.ClaudeBin,
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
		Model:        model,
		Effort:       effort,
		TimeoutMs:    s.config.TimeoutMS,
		Signal:       r.Context(),
		OnTextDelta: func(text string) {
			write(NewStreamChunk(id, created, model, StreamChunkDelta{"content": text}, nil, nil))
		},
	})

	if err != nil {
		errMsg := err.Error()
		write(map[string]interface{}{
			"error": map[string]interface{}{
				"message": errMsg,
				"type":    "api_error",
				"code":    nil,
				"param":   nil,
			},
		})
		return
	}

	*promptTokens = completion.Usage.InputTokens + completion.Usage.CacheReadInputTokens + completion.Usage.CacheCreationInputTokens
	*completionTokens = completion.Usage.OutputTokens

	usage := ToOpenAIUsage(completion.Usage, completion.CostUSD)
	finish := MapFinishReason(completion.StopReason)
	write(NewStreamChunk(id, created, completion.Model, StreamChunkDelta{}, &finish, &usage))
	fmt.Fprint(w, "data: [DONE]\n\n")
}

type toolResultMessage struct {
	toolCallID string
	content    string
}

// findContinuationSession collects the trailing tool messages of a request
// and correlates them to a live session by gateway-minted tool_call_id
// (ADR 0001 §6). Requests that match no live session fall through to
// cold-history flattening.
func (s *Server) findContinuationSession(messages []OpenAIMessage) (*Session, []toolResultMessage) {
	var results []toolResultMessage
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "tool" {
			break
		}
		text, err := textOf(messages[i])
		if err != nil {
			text = ""
		}
		results = append([]toolResultMessage{{toolCallID: messages[i].ToolCallID, content: text}}, results...)
	}

	for _, tr := range results {
		if sess := s.sessions.GetSessionByToolCallID(tr.toolCallID); sess != nil {
			return sess, results
		}
	}
	return nil, results
}

func toolChoiceIsNone(toolChoice interface{}) bool {
	choice, ok := toolChoice.(string)
	return ok && choice == "none"
}

type turnOutcome struct {
	text       string
	calls      []*PendingToolCall
	model      string
	stopReason *string
	usage      Usage
	costUSD    *float64
	final      bool
	err        error
}

// collectTurn drains session events until the turn ends: either the model
// announced tool calls and stopped with stop_reason "tool_use" (finalized
// from the stream alone — never from MCP traffic, ADR 0001 §4), or the
// whole run ended with a result line.
func (s *Server) collectTurn(ctx context.Context, sess *Session, onDelta func(string)) turnOutcome {
	var out turnOutcome
	sawToolStop := false
	timeout := time.NewTimer(time.Duration(s.config.TimeoutMS) * time.Millisecond)
	defer timeout.Stop()

	for {
		select {
		case ev, ok := <-sess.Events:
			if !ok {
				out.err = &ClaudeCliError{Message: "claude CLI stream ended unexpectedly", StatusCode: 502}
				return out
			}
			switch ev.Type {
			case "text":
				out.text += ev.Text
				if onDelta != nil {
					onDelta(ev.Text)
				}
			case "tool_calls":
				if ev.Model != "" {
					out.model = ev.Model
				}
				out.calls = append(out.calls, ev.Calls...)
			case "stop":
				if ev.StopReason != nil {
					out.stopReason = ev.StopReason
					if *ev.StopReason == "tool_use" {
						sawToolStop = true
					}
				}
				if ev.Usage != nil {
					out.usage = *ev.Usage
				}
			case "run_end":
				out.final = true
				if ev.Err != nil {
					out.err = ev.Err
					return out
				}
				if ev.ResultText != nil {
					out.text = *ev.ResultText
				}
				if ev.StopReason != nil {
					out.stopReason = ev.StopReason
				}
				if ev.Usage != nil {
					out.usage = *ev.Usage
				}
				out.costUSD = ev.CostUSD
				return out
			}
			if sawToolStop && len(out.calls) > 0 {
				return out
			}
		case <-timeout.C:
			out.err = &ClaudeCliError{
				Message:    fmt.Sprintf("claude CLI turn timed out after %dms", s.config.TimeoutMS),
				StatusCode: 504,
			}
			return out
		case <-ctx.Done():
			out.err = &ClaudeCliError{Message: "client disconnected", StatusCode: 499}
			return out
		}
	}
}

// handleToolTurn drives one turn of a tool-calling session: start a new
// session (contSess == nil) or resume a parked one, then respond with
// either tool_calls or the final completion. Returns the HTTP status for
// the request log.
func (s *Server) handleToolTurn(w http.ResponseWriter, r *http.Request, req ChatCompletionRequest, contSess *Session, toolResults []toolResultMessage, id string, created int64, model, effort string, promptTokens, completionTokens *int) int {
	sess := contSess

	if sess == nil {
		systemPrompt, prompt, err := BuildPrompt(req.Messages)
		if err != nil {
			s.sendError(w, 400, err.Error(), "invalid_request_error")
			return 400
		}

		sess = s.sessions.CreateSession(req.Tools)
		if sess == nil {
			s.sendError(w, 429, fmt.Sprintf("too many live tool sessions (max %d); retry later", s.config.MaxSessions), "rate_limit_error")
			return 429
		}

		err = StartClaudeSession(s.sessions, sess, SessionRunOptions{
			ClaudeBin:     s.config.ClaudeBin,
			Prompt:        prompt,
			SystemPrompt:  systemPrompt,
			Model:         model,
			Effort:        effort,
			MCPURL:        fmt.Sprintf("%s/mcp/%s", s.mcpBaseURL, sess.ID),
			ToolTimeoutMS: s.config.ToolTimeoutMS,
		})
		if err != nil {
			s.sessions.ReapSession(sess.ID)
			status := 500
			if cliErr, ok := err.(*ClaudeCliError); ok {
				status = cliErr.StatusCode
			}
			s.sendError(w, status, err.Error(), "api_error")
			return status
		}
	} else {
		// Resume: deliver the client's tool results to the parked calls.
		for _, tr := range toolResults {
			s.sessions.ResolveToolCall(sess, tr.toolCallID, tr.content)
		}
	}

	if req.Stream {
		return s.streamToolTurn(w, r, sess, id, created, model, promptTokens, completionTokens)
	}

	outcome := s.collectTurn(r.Context(), sess, nil)
	if outcome.model == "" {
		outcome.model = model
	}
	*promptTokens = outcome.usage.InputTokens + outcome.usage.CacheReadInputTokens + outcome.usage.CacheCreationInputTokens
	*completionTokens = outcome.usage.OutputTokens

	if outcome.err != nil {
		s.sessions.ReapSession(sess.ID)
		status := 500
		errType := "api_error"
		if cliErr, ok := outcome.err.(*ClaudeCliError); ok {
			status = cliErr.StatusCode
		}
		s.sendError(w, status, outcome.err.Error(), errType)
		return status
	}

	if outcome.final {
		s.sessions.ReapSession(sess.ID)
		completion := Completion{
			Text:       outcome.text,
			Model:      outcome.model,
			StopReason: outcome.stopReason,
			Usage:      outcome.usage,
			CostUSD:    outcome.costUSD,
		}
		s.sendJSON(w, 200, CompletionResponseWithCost(id, created, completion))
		return 200
	}

	// Tool-call turn: respond and leave the session (and its parked CLI
	// process) alive for the follow-up request. The concurrency slot is
	// released by the caller's defer when this response goes out.
	s.sendJSON(w, 200, NewToolCallResponse(id, created, outcome.model, outcome.text, outcome.calls, ToOpenAIUsage(outcome.usage, outcome.costUSD)))
	return 200
}

func (s *Server) streamToolTurn(w http.ResponseWriter, r *http.Request, sess *Session, id string, created int64, model string, promptTokens, completionTokens *int) int {
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(200)

	write := func(data interface{}) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	write(NewStreamChunk(id, created, model, StreamChunkDelta{"role": "assistant", "content": ""}, nil, nil))

	outcome := s.collectTurn(r.Context(), sess, func(text string) {
		write(NewStreamChunk(id, created, model, StreamChunkDelta{"content": text}, nil, nil))
	})
	if outcome.model == "" {
		outcome.model = model
	}

	if outcome.err != nil {
		s.sessions.ReapSession(sess.ID)
		write(map[string]interface{}{
			"error": map[string]interface{}{
				"message": outcome.err.Error(),
				"type":    "api_error",
				"code":    nil,
				"param":   nil,
			},
		})
		return 200
	}

	*promptTokens = outcome.usage.InputTokens + outcome.usage.CacheReadInputTokens + outcome.usage.CacheCreationInputTokens
	*completionTokens = outcome.usage.OutputTokens
	usage := ToOpenAIUsage(outcome.usage, outcome.costUSD)

	if outcome.final {
		s.sessions.ReapSession(sess.ID)
		finish := MapFinishReason(outcome.stopReason)
		write(NewStreamChunk(id, created, outcome.model, StreamChunkDelta{}, &finish, &usage))
		fmt.Fprint(w, "data: [DONE]\n\n")
		return 200
	}

	// Announce the tool calls as one delta chunk (arguments arrive whole),
	// then the terminal tool_calls chunk.
	deltaCalls := make([]map[string]interface{}, len(outcome.calls))
	for i, call := range outcome.calls {
		deltaCalls[i] = map[string]interface{}{
			"index": i,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]interface{}{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}
	}
	write(NewStreamChunk(id, created, outcome.model, StreamChunkDelta{"tool_calls": deltaCalls}, nil, nil))
	finish := "tool_calls"
	write(NewStreamChunk(id, created, outcome.model, StreamChunkDelta{}, &finish, &usage))
	fmt.Fprint(w, "data: [DONE]\n\n")
	return 200
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var models []ModelInfo
	for _, id := range s.config.Models {
		models = append(models, ModelInfo{
			ID:      id,
			Object:  "model",
			Created: 0,
			OwnedBy: "anthropic",
		})
	}

	s.sendJSON(w, 200, ModelsResponse{
		Object: "list",
		Data:   models,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(200)

	s.metrics.WritePrometheus(w)

	fmt.Fprintf(w, "# HELP llmgateway_live_sessions Number of live tool-calling CLI sessions (active + parked)\n")
	fmt.Fprintf(w, "# TYPE llmgateway_live_sessions gauge\n")
	fmt.Fprintf(w, "llmgateway_live_sessions %d\n", s.sessions.GetSessionCount())
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		s.handleHealthz(w, r)
		return
	}

	// The MCP relay endpoint is called by the spawned CLI, which does not
	// carry the gateway API key; the unguessable 128-bit session id is the
	// credential (ADR 0001 §1).
	if strings.HasPrefix(r.URL.Path, "/mcp/") {
		sessionID := strings.TrimPrefix(r.URL.Path, "/mcp/")
		s.handleMCPEndpoint(w, r, sessionID)
		return
	}

	if !s.isAuthorized(r) {
		s.sendError(w, 401, "invalid or missing API key", "authentication_error")
		return
	}

	if r.Method == "GET" && r.URL.Path == "/v1/models" {
		s.handleModels(w, r)
		return
	}

	if r.Method == "POST" && r.URL.Path == "/v1/chat/completions" {
		s.handleChatCompletions(w, r)
		return
	}

	if r.Method == "GET" && r.URL.Path == "/metrics" {
		s.handleMetrics(w, r)
		return
	}

	s.sendError(w, 404, fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path), "not_found_error")
}

func (s *Server) ListenAndServe() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listeners = append(s.listeners, listener)

	log.Printf("valyrium listening on http://%s (default model: %s, concurrency: %d, auth: %s)",
		addr, s.config.DefaultModel, s.config.Concurrency, func() string {
			if s.config.APIKey != "" {
				return "api key required"
			}
			return "open"
		}())

	srv := &http.Server{
		Handler:      s,
		Addr:         addr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(s.doneCh)
	}()

	err = srv.Serve(listener)
	if err != http.ErrServerClosed {
		return err
	}

	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	for _, listener := range s.listeners {
		listener.Close()
	}
	select {
	case <-s.doneCh:
	case <-ctx.Done():
	}
	return nil
}

func newCompletionID() string {
	return fmt.Sprintf("chatcmpl-%s", randomHex(32))
}

func randomHex(length int) string {
	const hexChars = "0123456789abcdef"
	b := make([]byte, length)
	for i := range b {
		b[i] = hexChars[i%len(hexChars)]
	}
	return string(b)
}
