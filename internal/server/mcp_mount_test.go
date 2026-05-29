package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/service"
)

func newMountTestServer(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	if cfg.MediaRoot == "" {
		cfg.MediaRoot = t.TempDir()
	}
	svc, err := service.New(cfg, store)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	return New(cfg, store, svc).Routes()
}

// postMCP sends a minimal JSON-RPC initialize to /mcp. We only care about the
// HTTP status (specifically whether the auth middleware let us through), not
// the MCP body.
func postMCP(t *testing.T, h http.Handler, bearer string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestMCPMountedWithoutAuth(t *testing.T) {
	h := newMountTestServer(t, &config.Config{}) // no AuthToken
	if code := postMCP(t, h, ""); code == http.StatusNotFound {
		t.Fatalf("/mcp not mounted (404)")
	} else if code == http.StatusUnauthorized {
		t.Fatalf("got 401 with no auth configured, want pass-through")
	}
}

func TestMCPAuthRejectsMissingAndWrongToken(t *testing.T) {
	h := newMountTestServer(t, &config.Config{AuthToken: "s3cret"})

	if code := postMCP(t, h, ""); code != http.StatusUnauthorized {
		t.Fatalf("missing token: code = %d, want 401", code)
	}
	if code := postMCP(t, h, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code = %d, want 401", code)
	}
	if code := postMCP(t, h, "s3cret"); code == http.StatusUnauthorized {
		t.Fatalf("correct token still 401")
	}
}

func TestHealthzBypassesAuth(t *testing.T) {
	h := newMountTestServer(t, &config.Config{AuthToken: "s3cret"})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz code = %d, want 200 (auth must not gate health)", rec.Code)
	}
}
