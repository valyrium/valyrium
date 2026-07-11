package gateway

import (
	_ "embed"
	"net/http"
)

// The dashboard is the finished design artifact in docs/design, copied here
// byte-for-byte because go:embed cannot reach outside the package's own
// directory tree (ADR 0003). It is served verbatim: it carries no data, only
// markup, and fetches /metrics and /v1/models itself.

//go:embed static/dashboard.html
var dashboardHTML []byte

//go:embed static/logo-mark.svg
var logoMarkSVG []byte

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(dashboardHTML)
}

func (s *Server) handleLogoMark(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "image/svg+xml")
	w.WriteHeader(200)
	_, _ = w.Write(logoMarkSVG)
}
