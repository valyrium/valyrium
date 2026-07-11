package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/valyrium/valyrium/internal/gateway"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("valyrium %s\n", version)
		return
	}

	port := 8787
	if p, err := strconv.Atoi(os.Getenv("CLAUDE_GATEWAY_PORT")); err == nil {
		port = p
	}

	host := os.Getenv("CLAUDE_GATEWAY_HOST")
	if host == "" {
		host = "127.0.0.1"
	}

	apiKey := os.Getenv("CLAUDE_GATEWAY_API_KEY")

	defaultModel := os.Getenv("CLAUDE_GATEWAY_MODEL")
	if defaultModel == "" {
		defaultModel = "sonnet"
	}

	modelsStr := os.Getenv("CLAUDE_GATEWAY_MODELS")
	if modelsStr == "" {
		modelsStr = "sonnet,opus,haiku"
	}
	models := make([]string, 0)
	for _, m := range strings.Split(modelsStr, ",") {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}

	claudeBin := os.Getenv("CLAUDE_GATEWAY_BIN")
	if claudeBin == "" {
		claudeBin = "claude"
	}

	timeoutMS := 300000
	if t, err := strconv.Atoi(os.Getenv("CLAUDE_GATEWAY_TIMEOUT_MS")); err == nil {
		timeoutMS = t
	}

	concurrency := 4
	if c, err := strconv.Atoi(os.Getenv("CLAUDE_GATEWAY_CONCURRENCY")); err == nil {
		concurrency = c
	}

	maxSessions := 16
	if m, err := strconv.Atoi(os.Getenv("CLAUDE_GATEWAY_MAX_SESSIONS")); err == nil {
		maxSessions = m
	}

	toolTimeoutMS := 120000
	if t, err := strconv.Atoi(os.Getenv("CLAUDE_GATEWAY_TOOL_TIMEOUT_MS")); err == nil {
		toolTimeoutMS = t
	}

	sessionIdleMS := 600000
	if t, err := strconv.Atoi(os.Getenv("CLAUDE_GATEWAY_SESSION_IDLE_MS")); err == nil {
		sessionIdleMS = t
	}

	// CLAUDE_GATEWAY_CONTEXT_LENGTH is either a bare integer (default for
	// ids with no known Claude family match) or a comma-separated
	// id=length list of per-model overrides, e.g. "opus=1000000,my-proxy=32000".
	contextLengths := make(map[string]int)
	defaultContextLength := 0
	if v := os.Getenv("CLAUDE_GATEWAY_CONTEXT_LENGTH"); v != "" {
		if strings.Contains(v, "=") {
			for _, pair := range strings.Split(v, ",") {
				id, lenStr, ok := strings.Cut(strings.TrimSpace(pair), "=")
				if !ok {
					continue
				}
				if n, err := strconv.Atoi(strings.TrimSpace(lenStr)); err == nil {
					contextLengths[strings.TrimSpace(id)] = n
				}
			}
		} else if n, err := strconv.Atoi(v); err == nil {
			defaultContextLength = n
		}
	}

	config := gateway.Config{
		Port:                 port,
		Host:                 host,
		APIKey:               apiKey,
		DefaultModel:         defaultModel,
		Models:               models,
		ClaudeBin:            claudeBin,
		TimeoutMS:            timeoutMS,
		Concurrency:          concurrency,
		MaxSessions:          maxSessions,
		ToolTimeoutMS:        toolTimeoutMS,
		SessionIdleMS:        sessionIdleMS,
		ContextLengths:       contextLengths,
		DefaultContextLength: defaultContextLength,
	}

	server := gateway.NewServer(config)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
