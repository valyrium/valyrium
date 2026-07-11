package gateway

import (
	"bytes"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newDashboardServer(t *testing.T, apiKey string) *Server {
	t.Helper()
	return NewServer(Config{
		Host:         "127.0.0.1",
		APIKey:       apiKey,
		DefaultModel: "sonnet",
		Models:       []string{"sonnet", "opus"},
		Concurrency:  4,
		UsageDB:      "off",
	})
}

func TestDashboardRouteServesHTML(t *testing.T) {
	server := newDashboardServer(t, "")

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("GET /dashboard: expected status 200, got %d", w.Code)
	}
	if ct := w.Header().Get("content-type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected content-type text/html; charset=utf-8, got %q", ct)
	}
	if body := w.Body.String(); !strings.HasPrefix(strings.ToLower(body), "<!doctype html>") {
		t.Errorf("expected an HTML document, got %q", truncate(body, 80))
	}
}

// The shell must load on a plain browser navigation, which cannot set an
// Authorization header — so it sits ahead of the auth check (ADR 0003).
func TestDashboardRouteBypassesAuth(t *testing.T) {
	server := newDashboardServer(t, "secret-key")

	// A key-less request to an authenticated route must still 401, or this
	// test proves nothing about the dashboard.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("GET /v1/models without a key: expected status 401, got %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/dashboard", nil)
	w = httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /dashboard without a key: expected status 200, got %d", w.Code)
	}
	if len(w.Body.Bytes()) == 0 {
		t.Error("expected the dashboard body, got nothing")
	}
}

func TestDashboardServesEmbeddedFileVerbatim(t *testing.T) {
	design, err := os.ReadFile("../../docs/design/dashboard.html")
	if err != nil {
		t.Fatalf("read design source: %v", err)
	}

	server := newDashboardServer(t, "")
	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if !bytes.Equal(w.Body.Bytes(), design) {
		t.Errorf("served bytes differ from docs/design/dashboard.html (%d served vs %d design)",
			len(w.Body.Bytes()), len(design))
	}

	logo, err := os.ReadFile("../../docs/design/logo-mark.svg")
	if err != nil {
		t.Fatalf("read logo source: %v", err)
	}
	if !bytes.Equal(logoMarkSVG, logo) {
		t.Error("embedded logo-mark.svg differs from docs/design/logo-mark.svg")
	}
}

func TestHealthzRecordsRequest(t *testing.T) {
	server := newDashboardServer(t, "")

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET /healthz: expected status 200, got %d", w.Code)
	}

	assertRecorded(t, server, "GET /healthz", 200, 1)
}

func TestModelsRecordsRequest(t *testing.T) {
	server := newDashboardServer(t, "")

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("GET /v1/models: expected status 200, got %d", w.Code)
		}
	}

	assertRecorded(t, server, "GET /v1/models", 200, 2)
}

func TestMetricsRecordsRequest(t *testing.T) {
	server := newDashboardServer(t, "")

	// The first scrape records itself but cannot report itself: the count is
	// written by a defer that runs after the body is composed. The second
	// scrape is where it shows up.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/metrics", nil)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("GET /metrics: expected status 200, got %d", w.Code)
		}
		if i == 1 && !strings.Contains(w.Body.String(), `llmgateway_requests_total{path="GET /metrics",status="200"} 1`) {
			t.Errorf("second scrape did not report the first one:\n%s", w.Body.String())
		}
	}

	assertRecorded(t, server, "GET /metrics", 200, 2)
}

func assertRecorded(t *testing.T, server *Server, route string, status int, want int64) {
	t.Helper()

	server.metrics.mu.RLock()
	defer server.metrics.mu.RUnlock()

	statuses, ok := server.metrics.requestsTotal[route]
	if !ok {
		t.Fatalf("no requests recorded for %q; recorded routes: %v", route, routeKeys(server.metrics))
	}
	if got := statuses[status]; got != want {
		t.Errorf("%q status %d: recorded %d requests, want %d", route, status, got, want)
	}
}

func routeKeys(m *Metrics) []string {
	keys := make([]string, 0, len(m.requestsTotal))
	for key := range m.requestsTotal {
		keys = append(keys, key)
	}
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
