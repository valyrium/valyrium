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
	Port         int
	Host         string
	APIKey       string
	DefaultModel string
	Models       []string
	ClaudeBin    string
	TimeoutMS    int
	Concurrency  int
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
	config    Config
	slots     *Semaphore
	metrics   *Metrics
	listeners []net.Listener
	doneCh    chan struct{}
}

func NewServer(config Config) *Server {
	return &Server{
		config:  config,
		slots:   NewSemaphore(config.Concurrency),
		metrics: NewMetrics(),
		doneCh:  make(chan struct{}),
	}
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
			"method":             r.Method,
			"path":               r.URL.Path,
			"model":              model,
			"status":             statusCode,
			"duration_ms":        duration.Milliseconds(),
			"prompt_tokens":      promptTokens,
			"completion_tokens":  completionTokens,
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

	systemPrompt, prompt, err := BuildPrompt(req.Messages)
	if err != nil {
		statusCode = 400
		s.sendError(w, 400, err.Error(), "invalid_request_error")
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

	if err = s.slots.Acquire(r.Context()); err != nil {
		statusCode = 503
		s.sendError(w, 503, "service temporarily unavailable", "api_error")
		return
	}
	defer s.slots.Release()

	if req.Stream {
		s.handleStreamingResponse(w, r, id, created, model, effort, systemPrompt, prompt)
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

func (s *Server) handleStreamingResponse(w http.ResponseWriter, r *http.Request, id string, created int64, model, effort, systemPrompt, prompt string) {
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

	usage := ToOpenAIUsage(completion.Usage, completion.CostUSD)
	write(NewStreamChunk(id, created, completion.Model, StreamChunkDelta{}, nil, &usage))
	fmt.Fprint(w, "data: [DONE]\n\n")
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
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		s.handleHealthz(w, r)
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

	log.Printf("llm-gateway listening on http://%s (default model: %s, concurrency: %d, auth: %s)",
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
