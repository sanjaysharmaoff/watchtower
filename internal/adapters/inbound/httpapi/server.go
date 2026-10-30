// Package httpapi is the inbound edge: the Alertmanager webhook and the SRE
// feedback endpoints. It depends only on sinks and decoders, so it has no idea
// what happens to an alert after it hands it over.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/config"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

// Deps is everything the HTTP layer needs, all of it a port.
type Deps struct {
	Config   config.HTTPConfig
	Log      ports.Logger
	Clock    ports.Clock
	Decoder  ports.AlertDecoder
	Alerts   ports.AlertSink
	Feedback ports.FeedbackSink
	Signer   ports.Signer
	Version  string

	// Ready is an optional readiness probe supplied by the composition root.
	Ready func(ctx context.Context) error
	// Stats is an optional counter source for the status endpoint.
	Stats func() map[string]int64
}

// Server owns the HTTP listener.
type Server struct {
	deps Deps
	srv  *http.Server
	log  ports.Logger
}

// New builds the server and its routes.
func New(d Deps) *Server {
	s := &Server{deps: d, log: d.Log.With("component", "http")}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /alerts", s.handleAlerts)
	mux.HandleFunc("POST /runbook/approve", s.handleFeedbackJSON)
	mux.HandleFunc("GET /runbook/approve", s.handleFeedbackLink)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /status", s.handleStatus)

	handler := s.withRecover(s.withLogging(s.withAuth(mux)))

	s.srv = &http.Server{
		Addr:              d.Config.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       d.Config.ReadTimeout,
		WriteTimeout:      d.Config.WriteTimeout,
		IdleTimeout:       d.Config.IdleTimeout,
	}
	return s
}

// Start begins serving and blocks until the listener stops.
func (s *Server) Start() error {
	s.log.Info("http listening", "addr", s.srv.Addr, "auth", s.deps.Config.AuthToken != "")
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// withAuth enforces a bearer token on machine endpoints when one is configured.
//
// GET /runbook/approve is deliberately exempt: it is clicked from an email client
// that cannot set headers, and it carries its own HMAC signature and expiry, which
// is what authenticates it.
func (s *Server) withAuth(next http.Handler) http.Handler {
	token := strings.TrimSpace(s.deps.Config.AuthToken)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || isSignedLink(r) || isPublic(r) {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if provided == "" {
			provided = strings.TrimSpace(r.Header.Get("X-WatchTower-Token"))
		}
		if !constantTimeEqual(provided, token) {
			s.log.Warn("unauthenticated request rejected", "path", r.URL.Path, "remote", clientIP(r))
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSignedLink(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/runbook/approve"
}

func isPublic(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz":
		return true
	default:
		return false
	}
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"took", time.Since(start).Round(time.Millisecond).String(), "remote", clientIP(r))
	})
}

func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("handler panic", "path", r.URL.Path, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, ok := strings.Cut(r.RemoteAddr, ":")
	if !ok {
		return r.RemoteAddr
	}
	return host
}

// constantTimeEqual compares tokens without leaking length-independent timing.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
