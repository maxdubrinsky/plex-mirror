package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/service"
)

// newTestServer wires a real service over a fresh DB and a temp media root so
// StorageStats' statfs succeeds. No Plex/Jellyfin creds → no sources, engine
// disabled; the portal chrome, auth, and storage/queue pages still work.
func newTestServer(t *testing.T, token string) (http.Handler, *db.Store, *config.Config) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{MediaRoot: t.TempDir(), AuthToken: token}
	svc, err := service.New(cfg, store)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	return server(t, cfg, store, svc).Routes(), store, cfg
}

func server(t *testing.T, cfg *config.Config, store *db.Store, svc *service.Service) *Server {
	t.Helper()
	return New(cfg, store, svc)
}

func TestPortalAuthRedirectsWhenUnauthenticated(t *testing.T) {
	h, _, _ := newTestServer(t, "s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/browse", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("Location = %q, want /login", loc)
	}
}

func TestPortalAuthAllowsWithCookie(t *testing.T) {
	h, _, _ := newTestServer(t, "s3cret")
	req := httptest.NewRequest(http.MethodGet, "/browse", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Browse") {
		t.Fatalf("body missing Browse chrome:\n%s", rec.Body.String())
	}
}

func TestPortalOpenWhenNoToken(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/browse", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (auth disabled)", rec.Code)
	}
}

func TestStaticServedWithoutAuth(t *testing.T) {
	h, _, _ := newTestServer(t, "s3cret")
	// The font path guards the //go:embed static directive recursing into the
	// fonts subdir — the portal's self-hosted typography must work offline.
	for _, path := range []string{
		"/static/app.css",
		"/static/htmx.min.js",
		"/static/fonts/ibm-plex-mono-latin-400.woff2",
		// Favicons / PWA assets must load on the (unauthenticated) login page too.
		"/static/icons/favicon.svg",
		"/static/icons/favicon-180.png",
		"/static/site.webmanifest",
		"/favicon.ico",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (assets must load for the login page)", path, rec.Code)
		}
	}
}

func TestLoginSetsCookieThenRejectsBadToken(t *testing.T) {
	h, _, _ := newTestServer(t, "s3cret")

	// Correct token → cookie + redirect to /browse.
	form := url.Values{"token": {"s3cret"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("good login status = %d, want 303", rec.Code)
	}
	var setCookie bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookieName && c.Value == "s3cret" && c.HttpOnly {
			setCookie = true
		}
	}
	if !setCookie {
		t.Fatalf("expected HttpOnly auth cookie to be set")
	}

	// Wrong token → 401, no cookie.
	bad := url.Values{"token": {"nope"}}
	req2 := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(bad.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401", rec2.Code)
	}
}

func TestQueueRowsEmpty(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/queue/rows", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nothing in flight") {
		t.Fatalf("expected empty-queue message, got:\n%s", rec.Body.String())
	}
}

func TestStorageEvictFlow(t *testing.T) {
	h, store, cfg := newTestServer(t, "")

	// Seed a ready item with a real file under the media root so evict has
	// something to delete and the gauge has usage to drop.
	file := filepath.Join(cfg.MediaRoot, "movies", "Test Movie (2021).mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(file, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	ctx := context.Background()
	if _, err := store.ExecContext(ctx, `
		INSERT INTO items (source, source_key, title, container, size_bytes, local_path,
			status, bytes_done, completed_at)
		VALUES ('plex','rk1','Test Movie','mkv',4096,?, 'ready',4096, unixepoch())`, file); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	var id int64
	if err := store.QueryRowContext(ctx, `SELECT id FROM items WHERE source_key='rk1'`).Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	// Storage page lists the item.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/storage", nil))
	if !strings.Contains(rec.Body.String(), "Test Movie") {
		t.Fatalf("storage page missing seeded item:\n%s", rec.Body.String())
	}

	// Evict it via the storage-panel target → panel re-renders without it.
	req := httptest.NewRequest(http.MethodPost, "/evict?id="+strconv.FormatInt(id, 10), nil)
	req.Header.Set("HX-Target", "storage-panel")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("evict status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "Test Movie") {
		t.Fatalf("evicted item still listed in panel:\n%s", rec2.Body.String())
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("expected file removed, stat err = %v", err)
	}
}

// The bulk-queue routes must be wired and degrade gracefully: with no sources
// configured, QueueContainer/PreviewContainer return a "not configured" error
// rendered as an inline 200 fragment (not a 404/500).
func TestQueueContainerRoutesWired(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/queue/container?source=plex&item=sh"},
		{http.MethodGet, "/queue/container/confirm?source=plex&item=sh"},
		{http.MethodGet, "/item/children?source=plex&id=sh"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status = %d, want 200 (inline error fragment); body=%s",
				c.method, c.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "not configured") {
			t.Errorf("%s %s: expected 'not configured' error, got:\n%s", c.method, c.path, rec.Body.String())
		}
	}
}

// The bulk-evict routes mirror the bulk-queue ones: wired and degrading to an
// inline "not configured" fragment (not a 404/500) when no source is configured.
func TestEvictContainerRoutesWired(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/evict/container?source=plex&item=sh"},
		{http.MethodGet, "/evict/container/confirm?source=plex&item=sh"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status = %d, want 200 (inline error fragment); body=%s",
				c.method, c.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "not configured") {
			t.Errorf("%s %s: expected 'not configured' error, got:\n%s", c.method, c.path, rec.Body.String())
		}
	}
}

func TestEvictUnknownIDShowsError(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/evict?id=9999", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (inline error)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("expected inline error fragment, got:\n%s", rec.Body.String())
	}
}
