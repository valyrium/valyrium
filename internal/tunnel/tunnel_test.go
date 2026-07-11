package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/valyrium/valyrium/internal/gateway"
)

// TestRelayRequiresTokenAtStartup settles open question 3 in ADR 0002: the
// relay hard-fails rather than starting up ready to hand traffic to whoever
// connects first.
func TestRelayRequiresTokenAtStartup(t *testing.T) {
	t.Run("no token is a startup failure", func(t *testing.T) {
		relay, err := NewRelay(RelayConfig{Domain: "relay.example.com"})
		if err == nil {
			t.Fatal("a relay with no token was allowed to start")
		}
		if relay != nil {
			t.Error("NewRelay returned a usable relay alongside the error")
		}
		if !strings.Contains(err.Error(), "VALYRIUM_TUNNEL_TOKEN") {
			t.Errorf("the error should name the variable to set, got %q", err)
		}
	})

	t.Run("a token is all a relay needs to exist", func(t *testing.T) {
		relay, err := NewRelay(RelayConfig{Token: "s3cret"})
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
		if relay.TunnelConnected() {
			t.Error("a relay nobody has dialed reports a tunnel client connected")
		}
	})

	// Both cases below are caught before anything binds, so neither can take
	// the port out from under a real relay.
	t.Run("serving public TLS also needs a domain", func(t *testing.T) {
		relay, err := NewRelay(RelayConfig{Token: "s3cret", CertCacheDir: t.TempDir()})
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
		err = relay.ListenAndServe()
		if err == nil {
			t.Fatal("a relay with no domain started serving anyway")
		}
		if !strings.Contains(err.Error(), "VALYRIUM_TUNNEL_DOMAIN") {
			t.Errorf("the error should name the variable to set, got %q", err)
		}
	})

	t.Run("certificates need somewhere to persist", func(t *testing.T) {
		relay, err := NewRelay(RelayConfig{Token: "s3cret", Domain: "relay.example.com"})
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
		err = relay.ListenAndServe()
		if err == nil {
			t.Fatal("a relay with nowhere to cache certificates started serving anyway")
		}
		if !strings.Contains(err.Error(), "VALYRIUM_TUNNEL_CERT_CACHE_DIR") {
			t.Errorf("the error should name the variable to set, got %q", err)
		}
	})
}

// TestRelayTLSConfigOffersTunnelALPN guards the production listener, which
// the tests above cannot use because it wants a certificate from Let's
// Encrypt. Everything downstream is demultiplexed by ALPN, so a listener that
// failed to offer vtun/1 would quietly turn every tunnel client into a public
// caller and 503 the lot.
func TestRelayTLSConfigOffersTunnelALPN(t *testing.T) {
	relay, err := NewRelay(RelayConfig{
		Token:        "s3cret",
		Domain:       "relay.example.com",
		CertCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	cfg, err := relay.tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}

	for _, proto := range []string{
		ALPNProto,      // tunnel clients
		"http/1.1",     // public callers, and the 503 is written as HTTP/1.1
		acme.ALPNProto, // autocert's TLS-ALPN-01 challenge
	} {
		if !slices.Contains(cfg.NextProtos, proto) {
			t.Errorf("the listener does not offer %q: %v", proto, cfg.NextProtos)
		}
	}

	// h2 would be piped fine as bytes, but the relay's own 503 is HTTP/1.1
	// and an h2 client could not read it.
	if slices.Contains(cfg.NextProtos, "h2") {
		t.Errorf("the listener offers h2, which the 503 path cannot answer: %v", cfg.NextProtos)
	}
	if cfg.GetCertificate == nil {
		t.Error("the listener has no certificate source")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("minimum TLS version is %#04x, want at least TLS 1.2", cfg.MinVersion)
	}
}

// TestRelayServes503WithoutTunnel covers the case the relay spends most of
// its life in when something is wrong at home: nothing to proxy to, so the
// caller is told so immediately instead of hanging (ADR 0002 §2).
func TestRelayServes503WithoutTunnel(t *testing.T) {
	cert, pool := newTestCert(t)
	relay, addr := startRelay(t, "s3cret", cert)

	if relay.TunnelConnected() {
		t.Fatal("no tunnel client has dialed in, but the relay says one has")
	}

	resp, err := publicClient(pool).Get("https://" + addr + "/v1/models")
	if err != nil {
		t.Fatalf("GET through the relay: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type: got %q, want application/json", got)
	}

	// The body is the gateway's own error envelope, so a client that already
	// parses valyrium errors can parse this one.
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("the 503 body was not a JSON error envelope: %v", err)
	}
	if !strings.Contains(body.Error.Message, "tunnel") {
		t.Errorf("the 503 should say no tunnel client is connected, got %q", body.Error.Message)
	}
}

// TestRelayRejectsBadToken is the check that keeps a stranger who can reach
// the port from registering as the tunnel endpoint and being handed traffic
// meant for the real gateway (ADR 0002 §5).
func TestRelayRejectsBadToken(t *testing.T) {
	cert, pool := newTestCert(t)
	relay, addr := startRelay(t, "the-real-token", cert)

	conn, err := dialControl(addr, pool)
	if err != nil {
		t.Fatalf("dialing the relay's control channel: %v", err)
	}
	defer func() { _ = conn.Close() }()

	err = authenticateTo(conn, "not-the-real-token")
	if err == nil {
		t.Fatal("the relay accepted a bad token")
	}
	if !strings.Contains(err.Error(), "rejected the token") {
		t.Errorf("unexpected rejection: %v", err)
	}

	// Long enough for a relay that registers first and authenticates later to
	// give itself away.
	time.Sleep(250 * time.Millisecond)
	if relay.TunnelConnected() {
		t.Fatal("the relay registered a client that failed authentication")
	}

	// The public side agrees: a rejected client is not something to proxy to.
	resp, err := publicClient(pool).Get("https://" + addr + "/v1/models")
	if err != nil {
		t.Fatalf("GET through the relay: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503 — a rejected client must never be proxied to", resp.StatusCode)
	}

	// The same handshake with the right token is accepted, so nothing above
	// passed for an unrelated reason.
	good, err := dialControl(addr, pool)
	if err != nil {
		t.Fatalf("dialing the relay's control channel: %v", err)
	}
	defer func() { _ = good.Close() }()

	if err := authenticateTo(good, "the-real-token"); err != nil {
		t.Fatalf("the relay rejected the right token: %v", err)
	}
	waitFor(t, "the relay to register the authenticated client", relay.TunnelConnected)
}

// TestTunnelEndToEnd is the whole point of ADR 0002: a public HTTPS caller
// reaches a gateway on a private network, over a connection that gateway
// dialed outward, and neither end has to know the other is behind a tunnel.
func TestTunnelEndToEnd(t *testing.T) {
	const (
		largeBody  = 512 * 1024 // several frames' worth
		streamGap  = 150 * time.Millisecond
		streamGaps = 2 // between chunk 1 and chunk 3
	)

	// Stands in for valyrium on the home network. Every route answers from
	// what it was sent, so the assertions read the response and never race
	// the handler for a variable.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"method": r.Method,
				"body":   string(body),
				"auth":   r.Header.Get("Authorization"),
			})

		case "/v1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 1; i <= 3; i++ {
				_, _ = fmt.Fprintf(w, "data: chunk-%d\n\n", i)
				w.(http.Flusher).Flush()
				if i < 3 {
					time.Sleep(streamGap)
				}
			}

		case "/v1/large":
			_, _ = w.Write(bytes.Repeat([]byte("v"), largeBody))

		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()

	cert, pool := newTestCert(t)
	relay, addr := startRelay(t, "s3cret", cert)
	startTunnel(t, addr, "s3cret", hostPort(origin.URL), pool)
	waitFor(t, "the tunnel client to register with the relay", relay.TunnelConnected)

	client := publicClient(pool)

	t.Run("a request reaches the gateway and its answer comes back", func(t *testing.T) {
		req, err := http.NewRequest("POST", "https://"+addr+"/v1/chat/completions",
			strings.NewReader(`{"model":"sonnet"}`))
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		// The relay never looks at this header; the gateway's own API key
		// check is what it is for, and it has to survive the pipe intact.
		req.Header.Set("Authorization", "Bearer gateway-key")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST through the relay: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d, want 200", resp.StatusCode)
		}

		var echo map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&echo); err != nil {
			t.Fatalf("decoding the gateway's answer: %v", err)
		}
		if echo["method"] != "POST" {
			t.Errorf("method: the gateway saw %q", echo["method"])
		}
		if echo["body"] != `{"model":"sonnet"}` {
			t.Errorf("request body: the gateway saw %q", echo["body"])
		}
		if echo["auth"] != "Bearer gateway-key" {
			t.Errorf("authorization: the gateway saw %q", echo["auth"])
		}
	})

	t.Run("a response larger than one frame arrives whole", func(t *testing.T) {
		resp, err := client.Get("https://" + addr + "/v1/large")
		if err != nil {
			t.Fatalf("GET through the relay: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the response: %v", err)
		}
		if len(body) != largeBody {
			t.Fatalf("got %d bytes, want %d — a response split across frames was not reassembled", len(body), largeBody)
		}
		if !bytes.Equal(body, bytes.Repeat([]byte("v"), largeBody)) {
			t.Error("the reassembled response does not match what was sent")
		}
	})

	t.Run("a streamed response is not buffered on the way through", func(t *testing.T) {
		// The relay pipes bytes rather than parsing HTTP precisely so that
		// streaming survives (ADR 0002, alternatives). If it buffered, all
		// three chunks would land at once at the end.
		resp, err := client.Get("https://" + addr + "/v1/stream")
		if err != nil {
			t.Fatalf("GET through the relay: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		reader := bufio.NewReader(resp.Body)
		arrivals := make([]time.Time, 0, 3)
		for i := 1; i <= 3; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("reading chunk %d: %v", i, err)
			}
			if want := fmt.Sprintf("data: chunk-%d\n", i); line != want {
				t.Fatalf("chunk %d: got %q, want %q", i, line, want)
			}
			arrivals = append(arrivals, time.Now())

			if _, err := reader.ReadString('\n'); err != nil { // the blank line
				t.Fatalf("reading the blank line after chunk %d: %v", i, err)
			}
		}

		spread := arrivals[2].Sub(arrivals[0])
		if floor := streamGaps * streamGap / 2; spread < floor {
			t.Errorf("all three chunks arrived within %s — the response was buffered rather than streamed", spread)
		}
	})
}

// TestTunnelPreservesGatewayAuth is the claim the whole security posture in
// ADR 0002 §5 rests on: the relay authenticates the tunnel, not the callers
// reaching it, so the gateway's own CLAUDE_GATEWAY_API_KEY has to be what
// still gates the API at the far end of the pipe. This runs the real gateway,
// not a stand-in, because that claim is about the real gateway.
func TestTunnelPreservesGatewayAuth(t *testing.T) {
	origin := httptest.NewServer(gateway.NewServer(gateway.Config{
		APIKey:       "gateway-key",
		DefaultModel: "sonnet",
		Models:       []string{"sonnet"},
		Concurrency:  1,
	}))
	defer origin.Close()

	cert, pool := newTestCert(t)
	relay, addr := startRelay(t, "s3cret", cert)
	startTunnel(t, addr, "s3cret", hostPort(origin.URL), pool)
	waitFor(t, "the tunnel client to register with the relay", relay.TunnelConnected)

	client := publicClient(pool)

	// The relay pipes an unauthenticated request through without comment; the
	// gateway is what turns it away.
	resp, err := client.Get("https://" + addr + "/v1/models")
	if err != nil {
		t.Fatalf("GET through the relay: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request got %d through the tunnel, want 401 — the tunnel is bypassing the gateway's API key", resp.StatusCode)
	}

	// The same request with the key, down the same pipe, reaches the gateway.
	req, err := http.NewRequest("GET", "https://"+addr+"/v1/models", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer gateway-key")

	authed, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET through the relay: %v", err)
	}
	defer func() { _ = authed.Body.Close() }()

	if authed.StatusCode != http.StatusOK {
		t.Fatalf("an authenticated request got %d through the tunnel, want 200", authed.StatusCode)
	}

	var models struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(authed.Body).Decode(&models); err != nil {
		t.Fatalf("decoding the gateway's model list: %v", err)
	}
	if models.Object != "list" || len(models.Data) == 0 {
		t.Errorf("the gateway's model list did not survive the pipe: %+v", models)
	}
}

// TestTunnelReconnectsAfterDrop is the property that makes the tunnel worth
// running unattended at home: the control connection dies — relay restart,
// ISP blip, NAT rebind — and the client dials back in on its own.
func TestTunnelReconnectsAfterDrop(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "alive")
	}))
	defer origin.Close()

	cert, pool := newTestCert(t)
	relay, addr := startRelay(t, "s3cret", cert)
	startTunnel(t, addr, "s3cret", hostPort(origin.URL), pool)
	waitFor(t, "the tunnel client to register with the relay", relay.TunnelConnected)

	client := publicClient(pool)
	if got := getBody(t, client, "https://"+addr+"/"); got != "alive" {
		t.Fatalf("before the drop: got %q, want %q", got, "alive")
	}

	// Pull the relay out from under the client, which is left holding a dead
	// control connection with nothing having told it so.
	_ = relay.Close()

	// A relay listening on the same address is a different relay: it can only
	// report a client if one dialed *it*, which is what proves the tunnel
	// reconnected rather than that the old connection somehow survived.
	restarted, _ := startRelayOn(t, addr, "s3cret", cert)
	if restarted.TunnelConnected() {
		t.Fatal("a freshly started relay already reports a tunnel client")
	}
	waitFor(t, "the tunnel client to reconnect to the restarted relay", restarted.TunnelConnected)

	client.CloseIdleConnections() // the pooled TLS connection died with the old relay
	if got := getBody(t, client, "https://"+addr+"/"); got != "alive" {
		t.Fatalf("after reconnecting: got %q, want %q", got, "alive")
	}
}

// --- helpers -----------------------------------------------------------

// newTestCert mints a certificate for the loopback address so the tests
// exercise the real TLS and ALPN paths without Let's Encrypt in the way.
func newTestCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "valyrium test relay"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the certificate: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func startRelay(t *testing.T, token string, cert tls.Certificate) (*Relay, string) {
	t.Helper()
	return startRelayOn(t, "127.0.0.1:0", token, cert)
}

// startRelayOn serves a relay on a loopback TLS listener offering the same
// ALPN protocols the production listener does, so the demux under test is the
// real one. Everything above that listener — auth, registration, the pipe —
// is production code.
func startRelayOn(t *testing.T, addr, token string, cert tls.Certificate) (*Relay, string) {
	t.Helper()

	relay, err := NewRelay(RelayConfig{Token: token, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{ALPNProto, "http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}

	go func() { _ = relay.Serve(ln) }()
	t.Cleanup(func() { _ = relay.Close() })

	return relay, ln.Addr().String()
}

// startTunnel runs a tunnel client against the relay for the rest of the
// test. The keepalive is short enough that every test using it also exercises
// the PING/PONG path.
func startTunnel(t *testing.T, relayAddr, token, localAddr string, pool *x509.CertPool) *Tunnel {
	t.Helper()

	tun, err := NewTunnel(TunnelConfig{
		RelayAddr:  relayAddr,
		Token:      token,
		LocalAddr:  localAddr,
		TLSConfig:  &tls.Config{RootCAs: pool},
		MinBackoff: 20 * time.Millisecond,
		MaxBackoff: 200 * time.Millisecond,
		KeepAlive:  50 * time.Millisecond,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tun.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
	return tun
}

// dialControl opens a control connection the way the tunnel client does: TLS
// carrying the tunnel ALPN protocol, which is what tells the relay this is
// not public traffic.
func dialControl(addr string, pool *x509.CertPool) (net.Conn, error) {
	return tls.Dial("tcp", addr, &tls.Config{
		RootCAs:    pool,
		NextProtos: []string{ALPNProto},
		MinVersion: tls.VersionTLS12,
	})
}

// publicClient is a caller arriving at the relay from the internet: real TLS,
// trusting only the test certificate, speaking HTTP/1.1 because that is all
// the relay offers.
func publicClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				NextProtos: []string{"http/1.1"},
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body of %s: %v", url, err)
	}
	return string(body)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hostPort(rawURL string) string {
	return strings.TrimPrefix(rawURL, "http://")
}

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
