package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
)

func TestHealthzOK(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	srv := New(&config.Config{}, store, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("status = %q, want ok", got["status"])
	}
}

func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for i := 0; i < 3; i++ {
		store, err := db.Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := store.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate %d: %v", i, err)
		}
		store.Close()
	}
}
