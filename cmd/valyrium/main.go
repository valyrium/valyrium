package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/valyrium/valyrium/internal/gateway"
	"github.com/valyrium/valyrium/internal/tunnel"
)

var version = "dev"

const usage = `valyrium — an OpenAI-compatible gateway for the Claude Code CLI

usage:
  valyrium [serve]    run the gateway (the default when no subcommand is given)
  valyrium relay      run the public relay that fronts a tunnel
  valyrium tunnel     dial a relay and forward its traffic to a local gateway
  valyrium --version  print the version

serve is configured with CLAUDE_GATEWAY_* environment variables, relay and
tunnel with VALYRIUM_TUNNEL_* ones. See docs/adr/0002-tunnel-relay.md.
`

// command is one subcommand, handed the arguments that follow its name.
type command func(args []string) error

func main() {
	if err := run(os.Args[1:], commands(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "valyrium: %v\n", err)
		os.Exit(1)
	}
}

func commands() map[string]command {
	return map[string]command{
		"serve":  runServe,
		"relay":  runRelay,
		"tunnel": runTunnel,
	}
}

// run dispatches argv to a subcommand. No arguments, or arguments that start
// with a flag, run the gateway: `valyrium` on its own started the gateway
// before subcommands existed and still does.
func run(args []string, cmds map[string]command, out io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-v", "version":
			_, _ = fmt.Fprintf(out, "valyrium %s\n", version)
			return nil
		case "--help", "-h", "help":
			_, _ = fmt.Fprint(out, usage)
			return nil
		}
	}

	name, rest := "serve", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, rest = args[0], args[1:]
	}

	cmd, ok := cmds[name]
	if !ok {
		return fmt.Errorf("unknown subcommand %q\n\n%s", name, usage)
	}
	return cmd(rest)
}

func runServe(args []string) error {
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

	resumeSessions := false
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_GATEWAY_RESUME"))) {
	case "1", "true", "yes", "on":
		resumeSessions = true
	}

	exposeReasoning := false
	if v, err := strconv.ParseBool(os.Getenv("CLAUDE_GATEWAY_EXPOSE_REASONING")); err == nil {
		exposeReasoning = v
	}

	config := gateway.Config{
		Port:                 envInt("CLAUDE_GATEWAY_PORT", 8787),
		Host:                 envStr("CLAUDE_GATEWAY_HOST", "127.0.0.1"),
		APIKey:               os.Getenv("CLAUDE_GATEWAY_API_KEY"),
		DefaultModel:         envStr("CLAUDE_GATEWAY_MODEL", "sonnet"),
		Models:               envList("CLAUDE_GATEWAY_MODELS", "sonnet,opus,haiku"),
		ClaudeBin:            envStr("CLAUDE_GATEWAY_BIN", "claude"),
		TimeoutMS:            envInt("CLAUDE_GATEWAY_TIMEOUT_MS", 300000),
		Concurrency:          envInt("CLAUDE_GATEWAY_CONCURRENCY", 4),
		MaxSessions:          envInt("CLAUDE_GATEWAY_MAX_SESSIONS", 16),
		ToolTimeoutMS:        envInt("CLAUDE_GATEWAY_TOOL_TIMEOUT_MS", 120000),
		SessionIdleMS:        envInt("CLAUDE_GATEWAY_SESSION_IDLE_MS", 600000),
		ContextLengths:       contextLengths,
		DefaultContextLength: defaultContextLength,
		ResumeSessions:       resumeSessions,
		ResumeMaxEntries:     envInt("CLAUDE_GATEWAY_RESUME_MAX", 32),
		ExposeReasoning:      exposeReasoning,
		// Empty means the default path ($HOME/.valyrium/usage.db); the
		// literal string "off" disables usage tracking entirely.
		UsageDB: os.Getenv("CLAUDE_GATEWAY_USAGE_DB"),
	}

	if err := gateway.NewServer(config).ListenAndServe(); err != nil {
		return fmt.Errorf("starting the gateway: %w", err)
	}
	return nil
}

// runRelay serves the public half of the tunnel: TLS on :443 with Let's
// Encrypt certificates, piping every inbound connection down to the
// registered tunnel client (docs/adr/0002-tunnel-relay.md §2).
func runRelay(args []string) error {
	relay, err := tunnel.NewRelay(tunnel.RelayConfig{
		Domain:       os.Getenv("VALYRIUM_TUNNEL_DOMAIN"),
		Token:        os.Getenv("VALYRIUM_TUNNEL_TOKEN"),
		CertCacheDir: envStr("VALYRIUM_TUNNEL_CERT_CACHE_DIR", defaultCertCacheDir()),
		Addr:         envStr("VALYRIUM_TUNNEL_LISTEN_ADDR", ":443"),
		HTTPAddr:     envStr("VALYRIUM_TUNNEL_HTTP_ADDR", ":80"),
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = relay.Close()
	}()

	if err := relay.ListenAndServe(); err != nil {
		return fmt.Errorf("starting the relay: %w", err)
	}
	return nil
}

// runTunnel dials the relay from inside the private network and forwards what
// it sends to the local gateway. The gateway is never told any of this is
// happening, and its own CLAUDE_GATEWAY_API_KEY is still the only thing
// gating the API: a tunnel in front of a gateway with no key set is an open
// gateway on the public internet (docs/adr/0002-tunnel-relay.md §5).
func runTunnel(args []string) error {
	client, err := tunnel.NewTunnel(tunnel.TunnelConfig{
		RelayAddr: os.Getenv("VALYRIUM_TUNNEL_RELAY_ADDR"),
		Token:     os.Getenv("VALYRIUM_TUNNEL_TOKEN"),
		LocalAddr: envStr("VALYRIUM_TUNNEL_LOCAL_ADDR", "127.0.0.1:8787"),
		// The relay's certificate is issued for the public hostname, which is
		// not necessarily the address dialed to reach it.
		TLSConfig: &tls.Config{ServerName: os.Getenv("VALYRIUM_TUNNEL_DOMAIN")},
	})
	if err != nil {
		return err
	}

	if os.Getenv("CLAUDE_GATEWAY_API_KEY") == "" {
		log.Print("warning: CLAUDE_GATEWAY_API_KEY is unset. The relay authenticates this tunnel, " +
			"not the callers reaching it, so a gateway with no API key set is an open gateway on the " +
			"public internet.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return client.Run(ctx)
}

// defaultCertCacheDir persists Let's Encrypt certificates across restarts;
// losing them means re-requesting on every start, which trips issuance rate
// limits quickly.
func defaultCertCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "valyrium-autocert"
	}
	return filepath.Join(dir, "valyrium", "autocert")
}

func envStr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return fallback
}

func envList(name, fallback string) []string {
	list := make([]string, 0)
	for _, item := range strings.Split(envStr(name, fallback), ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}
	return list
}
