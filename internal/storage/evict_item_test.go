package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEvictItem_RemovesFileAndFlipsStatus(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	path := writeFile(t, root, "movies/Encanto (2021).mkv", 1000)
	id := insertReady(t, store, "plex", "42", path, 1000, 100)
	mgr := NewManager(store, Policy{MediaRoot: root})

	ev, err := mgr.EvictItem(context.Background(), id)
	if err != nil {
		t.Fatalf("EvictItem: %v", err)
	}
	if ev.ID != id || ev.Reason != "manual" || ev.SizeBytes != 1000 {
		t.Fatalf("evicted record = %+v, want id=%d reason=manual size=1000", ev, id)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("file still present after evict: stat err = %v", statErr)
	}
	if got := statusOf(t, store, id); got != "evicted" {
		t.Fatalf("status = %q, want evicted", got)
	}
}

func TestEvictItem_NotFound(t *testing.T) {
	store := openTestStore(t)
	mgr := NewManager(store, Policy{MediaRoot: t.TempDir()})

	if _, err := mgr.EvictItem(context.Background(), 999); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}

// A bug elsewhere (or a tampered DB) could point local_path outside MediaRoot;
// EvictItem must refuse rather than become a delete-anywhere primitive.
func TestEvictItem_PathEscapeRejected(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "do-not-touch")
	if err := os.WriteFile(outside, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	id := insertReady(t, store, "plex", "evil", outside, 7, 100)
	mgr := NewManager(store, Policy{MediaRoot: root})

	if _, err := mgr.EvictItem(context.Background(), id); err == nil {
		t.Fatal("expected EvictItem to refuse a path outside MediaRoot")
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("outside file was removed/altered: %v", statErr)
	}
	// Row must remain 'ready' since we refused to act on it.
	if got := statusOf(t, store, id); got != "ready" {
		t.Fatalf("status = %q, want ready (unchanged)", got)
	}
}

// An already-evicted row has a NULL local_path; re-evicting must be a clean
// no-op rather than an error.
func TestEvictItem_IdempotentOnEvictedRow(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	path := writeFile(t, root, "movies/Up (2009).mkv", 500)
	id := insertReady(t, store, "plex", "9", path, 500, 100)
	mgr := NewManager(store, Policy{MediaRoot: root})

	if _, err := mgr.EvictItem(context.Background(), id); err != nil {
		t.Fatalf("first evict: %v", err)
	}
	ev, err := mgr.EvictItem(context.Background(), id)
	if err != nil {
		t.Fatalf("second evict: %v", err)
	}
	if ev.LocalPath != "" {
		t.Fatalf("second evict local_path = %q, want empty", ev.LocalPath)
	}
	if got := statusOf(t, store, id); got != "evicted" {
		t.Fatalf("status = %q, want evicted", got)
	}
}
