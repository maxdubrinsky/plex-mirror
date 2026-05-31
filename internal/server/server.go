package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/mcp"
	"github.com/maxdubrinsky/plex-mirror/internal/service"
)

type Server struct {
	cfg   *config.Config
	store *db.Store
	svc   *service.Service

	// nav memoizes per-source library lists for the sidebar so it isn't a fresh
	// network round-trip on every page render. See navLibraries / shell in
	// portal.go; cleared on settings reload.
	nav navCache
}

// New builds the HTTP server. svc may be nil (e.g. in tests that only exercise
// /healthz); when nil, the /mcp endpoint is not mounted.
func New(cfg *config.Config, store *db.Store, svc *service.Service) *Server {
	return &Server{cfg: cfg, store: store, svc: svc}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	if s.svc != nil {
		// Streamable HTTP serves all of GET/POST/DELETE on the single /mcp
		// endpoint, so register it without a method prefix and behind auth.
		mux.Handle("/mcp", s.requireAuth(mcp.Handler(s.svc)))

		// Public portal endpoints (no portal auth): the login form must be
		// reachable while unauthenticated, and the embedded assets back both it
		// and every authed page.
		mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
		mux.Handle("GET /favicon.ico", faviconHandler())
		mux.HandleFunc("GET /login", s.handleLoginGet)
		mux.HandleFunc("POST /login", s.handleLoginPost)
		mux.HandleFunc("GET /logout", s.handleLogout)
		mux.HandleFunc("POST /logout", s.handleLogout)

		// Everything else is the browser portal, behind cookie/bearer auth. The
		// "/" catch-all forwards to portalMux, which does its own method routing.
		mux.Handle("/", s.portalAuth(s.portalMux()))
	}
	return mux
}

// requireAuth enforces a static bearer token when one is configured. With no
// token set we assume Traefik forward-auth (or a trusted LAN) sits in front and
// pass requests through untouched. /healthz never routes through this.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, prefix) ||
			subtle.ConstantTimeCompare(
				[]byte(strings.TrimPrefix(got, prefix)),
				[]byte(s.cfg.AuthToken),
			) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	if err := s.store.PingContext(r.Context()); err != nil {
		status = "db unavailable: " + err.Error()
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
