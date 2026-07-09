package main

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/dndungu/llm-gateway/internal/gateway"
)

func main() {
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

	config := gateway.Config{
		Port:          port,
		Host:          host,
		APIKey:        apiKey,
		DefaultModel:  defaultModel,
		Models:        models,
		ClaudeBin:     claudeBin,
		TimeoutMS:     timeoutMS,
		Concurrency:   concurrency,
		MaxSessions:   maxSessions,
		ToolTimeoutMS: toolTimeoutMS,
		SessionIdleMS: sessionIdleMS,
	}

	server := gateway.NewServer(config)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
