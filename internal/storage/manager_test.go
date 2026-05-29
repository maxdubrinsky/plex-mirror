package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/db"
)

func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// writeFile creates a file under mediaRoot with the given size and returns its absolute path.
func writeFile(t *testing.T, mediaRoot, name string, size int64) string {
	t.Helper()
	full := filepath.Join(mediaRoot, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return full
}

// insertReady directly inserts a 'ready' row with a chosen completed_at so we
// can control eviction ordering deterministically without sleeping.
func insertReady(t *testing.T, store *db.Store, source, key, localPath string, size, completedAt int64) int64 {
	t.Helper()
	res, err := store.ExecContext(context.Background(), `
		INSERT INTO items (source, source_key, title, container, size_bytes, local_path,
			status, bytes_done, completed_at)
		VALUES (?, ?, ?, 'mkv', ?, ?, 'ready', ?, ?)
	`, source, key, key, size, localPath, size, completedAt)
	if err != nil {
		t.Fatalf("insert ready: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last id: %v", err)
	}
	return id
}

func statusOf(t *testing.T, store *db.Store, id int64) string {
	t.Helper()
	var s string
	if err := store.QueryRowContext(context.Background(),
		`SELECT status FROM items WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("status query: %v", err)
	}
	return s
}

func TestEvictNow_EmptyDB(t *testing.T) {
	store := openTestStore(t)
	mgr := NewManager(store, Policy{
		MediaRoot:    t.TempDir(),
		SoftCapBytes: 100,
		HardCapBytes: 200,
	})

	rep, err := mgr.EvictNow(context.Background(), false)
	if err != nil {
		t.Fatalf("EvictNow: %v", err)
	}
	if rep.ScannedItems != 0 || rep.UsedBytesBefore != 0 || rep.UsedBytesAfter != 0 || len(rep.EvictedItems) != 0 {
		t.Fatalf("expected zeroed report, got %+v", rep)
	}
}

func TestEvictNow_UnderSoftCap(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root, SoftCapBytes: 1_000_000})

	p := writeFile(t, root, "a.mkv", 100)
	insertReady(t, store, "plex", "a", p, 100, 1)

	rep, err := mgr.EvictNow(context.Background(), false)
	if err != nil {
		t.Fatalf("EvictNow: %v", err)
	}
	if len(rep.EvictedItems) != 0 {
		t.Fatalf("expected no evictions, got %+v", rep.EvictedItems)
	}
	if rep.UsedBytesBefore != 100 || rep.UsedBytesAfter != 100 {
		t.Fatalf("bytes mismatch: %+v", rep)
	}
}

func TestEvictNow_OverSoftCap_EvictsOldestFirst(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root, SoftCapBytes: 150})

	// Three 100-byte files, ascending completed_at.
	pa := writeFile(t, root, "a.mkv", 100)
	pb := writeFile(t, root, "b.mkv", 100)
	pc := writeFile(t, root, "c.mkv", 100)
	idA := insertReady(t, store, "plex", "a", pa, 100, 1) // oldest
	idB := insertReady(t, store, "plex", "b", pb, 100, 2)
	idC := insertReady(t, store, "plex", "c", pc, 100, 3) // newest

	rep, err := mgr.EvictNow(context.Background(), false)
	if err != nil {
		t.Fatalf("EvictNow: %v", err)
	}
	// 300 -> need to drop to <=150, so we must evict 2 items (oldest two).
	if len(rep.EvictedItems) != 2 {
		t.Fatalf("expected 2 evictions, got %d (%+v)", len(rep.EvictedItems), rep.EvictedItems)
	}
	if rep.EvictedItems[0].ID != idA || rep.EvictedItems[1].ID != idB {
		t.Fatalf("expected to evict A then B, got %+v", rep.EvictedItems)
	}
	if rep.UsedBytesAfter > 150 {
		t.Fatalf("after = %d, want <=150", rep.UsedBytesAfter)
	}

	// Files gone, statuses flipped, C still ready.
	if _, err := os.Stat(pa); !os.IsNotExist(err) {
		t.Fatalf("A still exists: %v", err)
	}
	if _, err := os.Stat(pb); !os.IsNotExist(err) {
		t.Fatalf("B still exists: %v", err)
	}
	if _, err := os.Stat(pc); err != nil {
		t.Fatalf("C should still exist: %v", err)
	}
	if statusOf(t, store, idA) != "evicted" || statusOf(t, store, idB) != "evicted" {
		t.Fatalf("A/B status not evicted")
	}
	if statusOf(t, store, idC) != "ready" {
		t.Fatalf("C status changed: %s", statusOf(t, store, idC))
	}
}

func TestEvictNow_OverHardCap_NoSoft_FallsBackTo90Pct(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	// Hard cap 1000, no soft → target 900. Usage 1200 → must drop to <=900.
	mgr := NewManager(store, Policy{MediaRoot: root, HardCapBytes: 1000})

	for i := range 12 {
		name := filepath.Join("dir", string(rune('a'+i))+".mkv")
		p := writeFile(t, root, name, 100)
		insertReady(t, store, "plex", string(rune('a'+i)), p, 100, int64(i+1))
	}

	rep, err := mgr.EvictNow(context.Background(), false)
	if err != nil {
		t.Fatalf("EvictNow: %v", err)
	}
	if rep.UsedBytesAfter > 900 {
		t.Fatalf("after = %d, want <=900", rep.UsedBytesAfter)
	}
	// Usage 1200, target 900 (90% of 1000 hard cap). Each evict drops 100 bytes;
	// 3 evictions take us from 1200 -> 900 which satisfies `running <= target`.
	if len(rep.EvictedItems) != 3 {
		t.Fatalf("expected 3 evictions, got %d", len(rep.EvictedItems))
	}
	if rep.EvictedItems[0].Reason != "over_hard_cap" {
		t.Fatalf("reason = %s, want over_hard_cap", rep.EvictedItems[0].Reason)
	}
}

func TestEvictNow_OverHardCap_UsesSoftCapTarget(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root, HardCapBytes: 1000, SoftCapBytes: 500})

	// 1200 bytes total, need to drop to <=500 (soft).
	for i := range 12 {
		name := string(rune('a'+i)) + ".mkv"
		p := writeFile(t, root, name, 100)
		insertReady(t, store, "plex", string(rune('a'+i)), p, 100, int64(i+1))
	}

	rep, err := mgr.EvictNow(context.Background(), false)
	if err != nil {
		t.Fatalf("EvictNow: %v", err)
	}
	if rep.UsedBytesAfter > 500 {
		t.Fatalf("after = %d, want <=500", rep.UsedBytesAfter)
	}
	if rep.EvictedItems[0].Reason != "over_hard_cap" {
		t.Fatalf("reason mismatch: %s", rep.EvictedItems[0].Reason)
	}
}

func TestEvictNow_DryRunDoesNotMutate(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root, SoftCapBytes: 150})

	pa := writeFile(t, root, "a.mkv", 100)
	pb := writeFile(t, root, "b.mkv", 100)
	pc := writeFile(t, root, "c.mkv", 100)
	idA := insertReady(t, store, "plex", "a", pa, 100, 1)
	idB := insertReady(t, store, "plex", "b", pb, 100, 2)
	insertReady(t, store, "plex", "c", pc, 100, 3)

	rep, err := mgr.EvictNow(context.Background(), true)
	if err != nil {
		t.Fatalf("EvictNow dry: %v", err)
	}
	if !rep.DryRun {
		t.Fatalf("DryRun flag not set")
	}
	if len(rep.EvictedItems) != 2 {
		t.Fatalf("dry plan should still list 2 items, got %d", len(rep.EvictedItems))
	}
	// Files still on disk, rows still 'ready'.
	for _, p := range []string{pa, pb, pc} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run removed %s: %v", p, err)
		}
	}
	if statusOf(t, store, idA) != "ready" || statusOf(t, store, idB) != "ready" {
		t.Fatalf("dry run mutated status")
	}
}

func TestEvictNow_PathEscapeRejected(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root, SoftCapBytes: 50})

	// Pre-create a sentinel outside root we can prove was not touched.
	outside := filepath.Join(t.TempDir(), "do-not-touch")
	if err := os.WriteFile(outside, []byte("sacred"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	idA := insertReady(t, store, "plex", "a", outside, 100, 1)

	rep, err := mgr.EvictNow(context.Background(), false)
	if err != nil {
		t.Fatalf("EvictNow: %v", err)
	}
	if len(rep.EvictedItems) != 0 {
		t.Fatalf("expected escape attempt to be skipped, got %+v", rep.EvictedItems)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was removed: %v", err)
	}
	if statusOf(t, store, idA) != "ready" {
		t.Fatalf("escape row should still be ready, got %s", statusOf(t, store, idA))
	}
}

func TestRecordReady_UpsertsAndBumpsStatus(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root})

	p1 := writeFile(t, root, "v1.mkv", 100)
	id1, err := mgr.RecordReady(context.Background(), ReadyItem{
		Source: "plex", SourceKey: "k1", Title: "T", Container: "mkv",
		LocalPath: p1, SizeBytes: 100,
	})
	if err != nil {
		t.Fatalf("RecordReady 1: %v", err)
	}

	// Flip to evicted to prove the upsert resurrects it.
	if _, err := store.ExecContext(context.Background(),
		`UPDATE items SET status='evicted', local_path=NULL WHERE id=?`, id1); err != nil {
		t.Fatalf("flip evicted: %v", err)
	}

	p2 := writeFile(t, root, "v2.mkv", 250)
	id2, err := mgr.RecordReady(context.Background(), ReadyItem{
		Source: "plex", SourceKey: "k1", Title: "T2", Container: "mkv",
		LocalPath: p2, SizeBytes: 250,
	})
	if err != nil {
		t.Fatalf("RecordReady 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("expected same id on upsert: %d vs %d", id1, id2)
	}

	var status, lp string
	var sz int64
	if err := store.QueryRowContext(context.Background(),
		`SELECT status, local_path, size_bytes FROM items WHERE id=?`, id1,
	).Scan(&status, &lp, &sz); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "ready" || lp != p2 || sz != 250 {
		t.Fatalf("upsert state wrong: status=%s lp=%s sz=%d", status, lp, sz)
	}
}

func TestMarkAccessed(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root})

	p := writeFile(t, root, "a.mkv", 10)
	id := insertReady(t, store, "plex", "a", p, 10, 1)

	if err := mgr.MarkAccessed(context.Background(), id); err != nil {
		t.Fatalf("MarkAccessed: %v", err)
	}
	var ts int64
	if err := store.QueryRowContext(context.Background(),
		`SELECT COALESCE(last_accessed, 0) FROM items WHERE id=?`, id).Scan(&ts); err != nil {
		t.Fatalf("query last_accessed: %v", err)
	}
	if ts == 0 {
		t.Fatalf("last_accessed not set")
	}
}

func TestUsedBytes(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root})

	if got, err := mgr.UsedBytes(context.Background()); err != nil || got != 0 {
		t.Fatalf("UsedBytes empty: got=%d err=%v", got, err)
	}

	pa := writeFile(t, root, "a.mkv", 100)
	pb := writeFile(t, root, "b.mkv", 50)
	insertReady(t, store, "plex", "a", pa, 100, 1)
	insertReady(t, store, "plex", "b", pb, 50, 2)

	// Also insert a non-ready row to confirm it's excluded.
	if _, err := store.ExecContext(context.Background(), `
		INSERT INTO items (source, source_key, title, size_bytes, status, queued_at)
		VALUES ('plex', 'q', 'q', 999, 'queued', 0)
	`); err != nil {
		t.Fatalf("insert queued: %v", err)
	}

	got, err := mgr.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %v", err)
	}
	if got != 150 {
		t.Fatalf("UsedBytes = %d, want 150", got)
	}
}

func TestFreeBytesAt(t *testing.T) {
	mgr := NewManager(nil, Policy{})
	free, err := mgr.FreeBytesAt(t.TempDir())
	if err != nil {
		t.Fatalf("FreeBytesAt: %v", err)
	}
	if free <= 0 {
		t.Fatalf("FreeBytesAt = %d, want > 0", free)
	}

	if _, err := mgr.FreeBytesAt("/nonexistent/definitely/not-a-path-xyz"); err == nil {
		t.Fatalf("expected error for nonexistent path")
	}
}

func TestRunSweeper_TriggersEvictionWhenOverCap(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	mgr := NewManager(store, Policy{MediaRoot: root, SoftCapBytes: 150})

	pa := writeFile(t, root, "a.mkv", 100)
	pb := writeFile(t, root, "b.mkv", 100)
	pc := writeFile(t, root, "c.mkv", 100)
	insertReady(t, store, "plex", "a", pa, 100, 1)
	insertReady(t, store, "plex", "b", pb, 100, 2)
	insertReady(t, store, "plex", "c", pc, 100, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		mgr.RunSweeper(ctx, 10*time.Millisecond)
		close(done)
	}()

	// Poll until usage drops below soft cap or context expires.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		used, err := mgr.UsedBytes(context.Background())
		if err != nil {
			t.Fatalf("UsedBytes: %v", err)
		}
		if used <= 150 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("sweeper did not stop after ctx cancel")
	}

	used, err := mgr.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %v", err)
	}
	if used > 150 {
		t.Fatalf("sweeper failed to evict: used=%d > soft=150", used)
	}
}

func TestRunSweeper_ZeroIntervalDisabled(t *testing.T) {
	mgr := NewManager(nil, Policy{})
	done := make(chan struct{})
	go func() {
		mgr.RunSweeper(context.Background(), 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("RunSweeper with zero interval should return immediately")
	}
}

func TestPathIsUnderRoot(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name      string
		candidate string
		want      bool
	}{
		{"under", filepath.Join(root, "a.mkv"), true},
		{"nested", filepath.Join(root, "sub", "a.mkv"), true},
		{"escape", "/etc/passwd", false},
		{"root_itself", root, false},
		{"parent", filepath.Dir(root), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := pathIsUnderRoot(root, tc.candidate)
			if err != nil && tc.want {
				t.Fatalf("unexpected err: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("ok=%v want=%v (err=%v)", ok, tc.want, err)
			}
		})
	}
}
