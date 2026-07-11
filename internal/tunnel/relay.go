package tunnel

import (
	"bufio"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// handshakeTimeout bounds the TLS handshake and the token exchange that
// follows it, so a connection that opens and then says nothing cannot hold
// a goroutine indefinitely.
const handshakeTimeout = 20 * time.Second

// errBadToken is what the relay reports internally when a client reaches the
// control channel with the wrong bearer token. The client is told nothing
// beyond the connection closing.
var errBadToken = errors.New("tunnel: invalid bearer token")

type RelayConfig struct {
	Domain       string // VALYRIUM_TUNNEL_DOMAIN: the public hostname the certificate is pinned to
	Token        string // VALYRIUM_TUNNEL_TOKEN: shared secret the tunnel client presents
	CertCacheDir string // VALYRIUM_TUNNEL_CERT_CACHE_DIR: autocert.DirCache path
	Addr         string // VALYRIUM_TUNNEL_LISTEN_ADDR: public TLS listen address, default :443
	HTTPAddr     string // VALYRIUM_TUNNEL_HTTP_ADDR: HTTP-01 challenge address, default :80
	Logger       *log.Logger
}

// Relay is the public half of the tunnel: it terminates TLS, demultiplexes
// tunnel clients from public callers by ALPN, and pipes each public
// connection down the registered tunnel client's mux (ADR 0002 §2).
type Relay struct {
	cfg  RelayConfig
	acme *autocert.Manager

	mu  sync.Mutex
	tun *Conn        // the single registered tunnel client (v1 is single-tenant)
	ln  net.Listener // nil until Serve is running

	closeOnce sync.Once
	closed    chan struct{}
}

// NewRelay validates the relay's configuration. It refuses to build a relay
// with no token: without one, anybody who can reach the port could register
// as the tunnel endpoint and be handed traffic meant for the real gateway
// (ADR 0002 §5, open question 3).
func NewRelay(cfg RelayConfig) (*Relay, error) {
	if cfg.Token == "" {
		return nil, errors.New("relay: VALYRIUM_TUNNEL_TOKEN must be set — a relay with no token would hand traffic to whoever connects first")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":443"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":80"
	}

	return &Relay{cfg: cfg, closed: make(chan struct{})}, nil
}

// TunnelConnected reports whether a tunnel client is currently registered.
// Public traffic is answered with 503 whenever it is false.
func (r *Relay) TunnelConnected() bool {
	return r.tunnel() != nil
}

// ListenAndServe serves public TLS on cfg.Addr with Let's Encrypt
// certificates, and runs the HTTP-01 challenge responder on cfg.HTTPAddr.
func (r *Relay) ListenAndServe() error {
	tlsConfig, err := r.tlsConfig()
	if err != nil {
		return err
	}

	go func() {
		// autocert answers /.well-known/acme-challenge/ here; everything else
		// on :80 is redirected to HTTPS.
		h := r.acme.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "https://"+r.cfg.Domain+req.URL.RequestURI(), http.StatusMovedPermanently)
		}))
		if err := http.ListenAndServe(r.cfg.HTTPAddr, h); err != nil {
			r.logf("http-01 challenge listener on %s stopped: %v", r.cfg.HTTPAddr, err)
		}
	}()

	ln, err := tls.Listen("tcp", r.cfg.Addr, tlsConfig)
	if err != nil {
		return err
	}

	r.logf("relay listening on %s for %s (certificates cached in %s)", r.cfg.Addr, r.cfg.Domain, r.cfg.CertCacheDir)
	return r.Serve(ln)
}

// Serve accepts connections on ln until the relay is closed. ln is expected
// to be a TLS listener whose config offers ALPNProto; connections that
// negotiate it are tunnel clients, and everything else is public traffic.
func (r *Relay) Serve(ln net.Listener) error {
	r.mu.Lock()
	r.ln = ln
	r.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-r.closed:
				return nil
			default:
			}
			return err
		}
		go r.handleConn(conn)
	}
}

// Close stops accepting and drops the registered tunnel client.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)

		r.mu.Lock()
		ln, tun := r.ln, r.tun
		r.tun = nil
		r.mu.Unlock()

		if ln != nil {
			_ = ln.Close()
		}
		if tun != nil {
			_ = tun.Close()
		}
	})
	return nil
}

func (r *Relay) handleConn(conn net.Conn) {
	// The ALPN protocol is only known once the handshake completes, and
	// tls.Conn defers it until the first read.
	if tc, ok := conn.(*tls.Conn); ok {
		_ = tc.SetDeadline(time.Now().Add(handshakeTimeout))
		if err := tc.Handshake(); err != nil {
			_ = conn.Close()
			return
		}
		_ = tc.SetDeadline(time.Time{})

		if tc.ConnectionState().NegotiatedProtocol == ALPNProto {
			r.serveTunnelClient(conn)
			return
		}
	}
	r.servePublic(conn)
}

// serveTunnelClient authenticates a control connection and, if it checks
// out, registers it as the mux every public connection is piped down.
func (r *Relay) serveTunnelClient(conn net.Conn) {
	if err := r.authenticate(conn); err != nil {
		r.logf("rejected tunnel client %s: %v", conn.RemoteAddr(), err)
		_ = conn.Close()
		return
	}

	mux := NewConn(conn, SideRelay)
	r.register(mux)
	r.logf("tunnel client registered from %s", conn.RemoteAddr())

	<-mux.Done()
	r.unregister(mux)
	r.logf("tunnel client %s disconnected: %v", conn.RemoteAddr(), mux.Err())
}

// authenticate reads the client's AUTH frame and compares the token in
// constant time. It reads unbuffered, straight off the connection, so no
// bytes of the mux stream that follows are swallowed into a buffer the mux
// will never see.
func (r *Relay) authenticate(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	f, err := ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("reading the auth frame: %w", err)
	}
	if f.Type != FrameAuth {
		return fmt.Errorf("expected an %s frame, got %s", FrameAuth, f.Type)
	}
	if subtle.ConstantTimeCompare(f.Payload, []byte(r.cfg.Token)) != 1 {
		return errBadToken
	}
	return WriteFrame(conn, Frame{Type: FrameAuthOK})
}

// register installs mux as the active tunnel. A second client with a valid
// token replaces the first (last-writer-wins), so a reconnect after a
// network blip is never locked out by the stale connection it is replacing
// (ADR 0002 §2).
func (r *Relay) register(mux *Conn) {
	r.mu.Lock()
	previous := r.tun
	r.tun = mux
	r.mu.Unlock()

	if previous != nil {
		r.logf("replacing the previously registered tunnel client")
		_ = previous.Close()
	}
}

// unregister clears mux only if it is still the registered one: a connection
// that has already been replaced must not clear its replacement on the way out.
func (r *Relay) unregister(mux *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tun == mux {
		r.tun = nil
	}
}

func (r *Relay) tunnel() *Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tun
}

// servePublic pipes one inbound public connection down the tunnel. Past the
// ALPN demux the relay adds no trust of its own: it opens a stream and
// copies bytes, and the gateway's own API key is what gates the API at the
// far end (ADR 0002 §5).
func (r *Relay) servePublic(conn net.Conn) {
	mux := r.tunnel()
	if mux == nil {
		r.writeUnavailable(conn)
		_ = conn.Close()
		return
	}

	stream, err := mux.Open()
	if err != nil {
		r.logf("opening a stream for %s: %v", conn.RemoteAddr(), err)
		r.writeUnavailable(conn)
		_ = conn.Close()
		return
	}

	pipe(conn, stream)
}

// writeUnavailable answers a public caller directly when there is nothing to
// proxy to. This is the one response the relay generates itself, so for this
// connection it has to behave like an HTTP server rather than a pipe:
//
//   - It reads the caller's request first. A response that arrives before the
//     request has been written is "unsolicited" to any HTTP client that pools
//     connections, which discards it and reports a transport error instead of
//     the 503 the operator needs to see.
//   - It drains the request body, so a caller still uploading is not reset
//     mid-write and can actually read the answer.
//   - It answers in HTTP/1.1, which is why the relay's TLS config offers
//     http/1.1 and not h2: the byte pipe is protocol-agnostic, but this
//     response is not.
func (r *Relay) writeUnavailable(conn net.Conn) {
	const (
		body = `{"error":{"message":"no valyrium tunnel client is connected to this relay","type":"api_error"}}`

		// A caller uploading more than this gets reset rather than a 503; the
		// alternative is reading a 32MB completion request just to throw it away.
		maxDrain = 1 << 20
	)

	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	if req, err := http.ReadRequest(bufio.NewReader(conn)); err == nil && req.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, maxDrain))
		_ = req.Body.Close()
	}

	// The caller is already being turned away; a write error here leaves
	// nothing further to tell them.
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 503 Service Unavailable\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n"+
		"\r\n%s", len(body), body)
}

func (r *Relay) tlsConfig() (*tls.Config, error) {
	if r.cfg.Domain == "" {
		return nil, errors.New("relay: VALYRIUM_TUNNEL_DOMAIN must be set to the public hostname the certificate is issued for")
	}
	if r.cfg.CertCacheDir == "" {
		return nil, errors.New("relay: VALYRIUM_TUNNEL_CERT_CACHE_DIR must be set, or every restart re-requests a certificate and trips Let's Encrypt rate limits")
	}

	r.acme = &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(r.cfg.Domain),
		Cache:      autocert.DirCache(r.cfg.CertCacheDir),
	}

	cfg := r.acme.TLSConfig()
	// autocert offers h2; the relay does not, because the 503 above is
	// written as HTTP/1.1. acme.ALPNProto has to stay for TLS-ALPN-01
	// challenges to keep working.
	cfg.NextProtos = []string{ALPNProto, "http/1.1", acme.ALPNProto}
	cfg.MinVersion = tls.VersionTLS12
	return cfg, nil
}

func (r *Relay) logf(format string, args ...interface{}) {
	if r.cfg.Logger != nil {
		r.cfg.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
