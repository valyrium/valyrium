package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"time"
)

const (
	defaultLocalAddr  = "127.0.0.1:8787"
	defaultMinBackoff = 1 * time.Second
	defaultMaxBackoff = 30 * time.Second

	// defaultKeepAlive has to stay well under idleTimeout on the far end,
	// and under the idle-connection timeout of any NAT or firewall between
	// here and the relay (ADR 0002 §3).
	defaultKeepAlive = 30 * time.Second

	dialTimeout = 20 * time.Second
)

type TunnelConfig struct {
	RelayAddr string // VALYRIUM_TUNNEL_RELAY_ADDR: host:port of the relay
	Token     string // VALYRIUM_TUNNEL_TOKEN: shared secret the relay checks
	LocalAddr string // VALYRIUM_TUNNEL_LOCAL_ADDR: gateway to forward to, default 127.0.0.1:8787

	// TLSConfig overrides how the relay's certificate is verified. Nil means
	// the system roots, which is what a real deployment wants.
	TLSConfig *tls.Config

	MinBackoff time.Duration
	MaxBackoff time.Duration
	KeepAlive  time.Duration
	Logger     *log.Logger
}

// Tunnel is the private half: it dials out to the relay, then serves every
// stream the relay opens by connecting to the local gateway and piping bytes
// between the two (ADR 0002 §3).
type Tunnel struct {
	cfg TunnelConfig
}

func NewTunnel(cfg TunnelConfig) (*Tunnel, error) {
	if cfg.RelayAddr == "" {
		return nil, errors.New("tunnel: VALYRIUM_TUNNEL_RELAY_ADDR must be set to the relay's host:port")
	}
	if cfg.Token == "" {
		return nil, errors.New("tunnel: VALYRIUM_TUNNEL_TOKEN must be set — the relay will not register a client without it")
	}
	if cfg.LocalAddr == "" {
		cfg.LocalAddr = defaultLocalAddr
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = defaultKeepAlive
	}

	return &Tunnel{cfg: cfg}, nil
}

// Run keeps a control connection to the relay up until ctx is cancelled,
// reconnecting with exponential backoff whenever it drops. The local gateway
// is untouched by any of this: it never learns the tunnel exists.
func (t *Tunnel) Run(ctx context.Context) error {
	backoff := t.cfg.MinBackoff

	for {
		start := time.Now()
		err := t.connectAndServe(ctx)

		if ctx.Err() != nil {
			return nil
		}

		// A connection that stood up and did real work has earned a fresh
		// backoff: only repeated *failures to establish* should back off.
		if time.Since(start) > t.cfg.MaxBackoff {
			backoff = t.cfg.MinBackoff
		}

		t.logf("tunnel to %s is down (%v); reconnecting in %s", t.cfg.RelayAddr, err, backoff)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}

		if backoff *= 2; backoff > t.cfg.MaxBackoff {
			backoff = t.cfg.MaxBackoff
		}
	}
}

// connectAndServe holds one control connection open, returning the error
// that ended it.
func (t *Tunnel) connectAndServe(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", t.cfg.RelayAddr, t.tlsConfig())
	if err != nil {
		return fmt.Errorf("dialing relay %s: %w", t.cfg.RelayAddr, err)
	}
	defer func() { _ = conn.Close() }()

	if err := authenticateTo(conn, t.cfg.Token); err != nil {
		return err
	}

	mux := NewConn(conn, SideTunnel)
	defer func() { _ = mux.Close() }()

	// Scoped to this connection, not to ctx: a watcher per reconnect that
	// only ever exits when the caller does would pile up one goroutine per
	// dropped connection.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go mux.Keepalive(t.cfg.KeepAlive)
	go func() {
		<-connCtx.Done()
		_ = mux.Close()
	}()

	t.logf("tunnel established to %s, forwarding to %s", t.cfg.RelayAddr, t.cfg.LocalAddr)

	for {
		stream, err := mux.Accept()
		if err != nil {
			return err
		}
		go t.forward(stream)
	}
}

// forward serves one inbound public connection by dialing the local gateway
// and piping the stream to it.
func (t *Tunnel) forward(stream *Stream) {
	local, err := net.DialTimeout("tcp", t.cfg.LocalAddr, dialTimeout)
	if err != nil {
		// Closing the stream propagates back to the relay, which drops the
		// public connection. The relay stays a dumb pipe: it is not the
		// tunnel's job to synthesize an HTTP error for a gateway that is not
		// listening.
		t.logf("dialing the local gateway at %s: %v", t.cfg.LocalAddr, err)
		_ = stream.Close()
		return
	}

	pipe(local, stream)
}

// authenticateTo presents the bearer token and waits for the relay to accept
// it. Like the relay's side of this exchange it reads and writes unbuffered,
// so the mux that follows starts on a clean connection.
func authenticateTo(conn net.Conn, token string) error {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	if err := WriteFrame(conn, Frame{Type: FrameAuth, Payload: []byte(token)}); err != nil {
		return fmt.Errorf("sending the auth frame: %w", err)
	}

	f, err := ReadFrame(conn)
	if err != nil {
		// A relay that dislikes the token closes without explanation, so an
		// EOF here is the expected shape of a rejection.
		return fmt.Errorf("the relay rejected the token or closed the connection: %w", err)
	}
	if f.Type != FrameAuthOK {
		return fmt.Errorf("expected an %s frame from the relay, got %s", FrameAuthOK, f.Type)
	}
	return nil
}

// tlsConfig always requests ALPNProto: a control connection that fails to
// negotiate it would be mistaken for public traffic and piped straight back
// into itself.
func (t *Tunnel) tlsConfig() *tls.Config {
	cfg := &tls.Config{}
	if t.cfg.TLSConfig != nil {
		cfg = t.cfg.TLSConfig.Clone()
	}

	cfg.NextProtos = []string{ALPNProto}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(t.cfg.RelayAddr); err == nil {
			cfg.ServerName = host
		}
	}
	return cfg
}

func (t *Tunnel) logf(format string, args ...interface{}) {
	if t.cfg.Logger != nil {
		t.cfg.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
