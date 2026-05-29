package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// fakeSource implements source.Source + source.DownloadResolver against a
// single in-memory item, with a pluggable URL so each test can point at its
// own httptest.Server.
type fakeSource struct {
	url   string
	item  source.Item
	calls int32 // counts ResolveDownloadURL invocations
}

func (f *fakeSource) Name() string { return "plex" }
func (f *fakeSource) ListLibraries(ctx context.Context) ([]source.Library, error) {
	return nil, nil
}
func (f *fakeSource) ListItems(ctx context.Context, _ string, _ source.ListOptions) ([]source.Item, error) {
	return []source.Item{f.item}, nil
}
func (f *fakeSource) GetMetadata(ctx context.Context, id string) (source.Item, error) {
	if id != f.item.ID {
		return source.Item{}, source.ErrNotFound
	}
	return f.item, nil
}
func (f *fakeSource) ResolveDownloadURL(ctx context.Context, id string) (*source.DownloadTarget, error) {
	if id != f.item.ID {
		return nil, source.ErrNotFound
	}
	atomic.AddInt32(&f.calls, 1)
	return &source.DownloadTarget{URL: f.url, Headers: http.Header{"X-Plex-Token": []string{"t"}}}, nil
}

// fakeScanner counts calls so tests can verify post-download refresh fired.
type fakeScanner struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *fakeScanner) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

// rangeHandler serves payload with HTTP Range support. cutAfter > 0 makes the
// handler hang up after writing cutAfter bytes of body, simulating a network
// drop mid-transfer (used to test resume).
type rangeHandler struct {
	payload  []byte
	cutAfter int64
	calls    int32
}

func (h *rangeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&h.calls, 1)
	total := int64(len(h.payload))
	start := int64(0)
	end := total - 1

	if rh := r.Header.Get("Range"); rh != "" {
		// Expect "bytes=N-" (open-ended) — that's what the engine sends.
		if !strings.HasPrefix(rh, "bytes=") {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		spec := strings.TrimPrefix(rh, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		if len(parts) == 0 {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		n, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if n >= total {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = n
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}

	body := h.payload[start:]
	if h.cutAfter > 0 && int64(len(body)) > h.cutAfter {
		body = body[:h.cutAfter]
		_, _ = w.Write(body)
		// Force connection close so the client sees EOF mid-stream.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack to slam the connection shut.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
		return
	}
	_, _ = w.Write(body)
}

// testEngine spins up a fresh DB + engine wired to the given handler and
// source. Returns the engine, fake source, and a temp media root cleaned up
// by t.Cleanup.
func testEngine(t *testing.T, handler http.Handler, payload []byte, scanner Scanner) (*Engine, *fakeSource, *httptest.Server, string) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	mediaRoot := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("db migrate: %v", err)
	}

	fs := &fakeSource{
		url: srv.URL + "/file.mkv",
		item: source.Item{
			ID: "42", Title: "Test", Kind: source.ItemMovie, Year: 2024,
			Container: "mkv", SizeBytes: int64(len(payload)),
		},
	}

	mgr := storage.NewManager(store, storage.Policy{MediaRoot: mediaRoot})
	eng, err := New(store, mgr, Options{
		MediaRoot:      mediaRoot,
		Concurrency:    2,
		Scanner:        scanner,
		SourceName:     "plex",
		Resolver:       fs,
		RetryAttempts:  3,
		RetryBaseDelay: 5 * time.Millisecond,
		ProgressEvery:  10 * time.Millisecond,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return eng, fs, srv, mediaRoot
}

func TestDownloadHappyPath(t *testing.T) {
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	h := &rangeHandler{payload: payload}
	scanner := &fakeScanner{}
	eng, fs, _, mediaRoot := testEngine(t, h, payload, scanner)

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := eng.Download(context.Background(), id); err != nil {
		t.Fatalf("download: %v", err)
	}

	final := filepath.Join(mediaRoot, "movies", "Test (2024).mkv")
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("final file content differs from payload")
	}

	// .partials should be empty after rename.
	partial := eng.layout.Partial("plex", fs.item.ID)
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial still exists: %v", err)
	}

	if scanner.calls != 1 {
		t.Errorf("scanner.Refresh called %d times, want 1", scanner.calls)
	}
}

func TestDownloadResumeAfterKill(t *testing.T) {
	payload := make([]byte, 16*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	// First server: hang up after 4 KiB.
	h1 := &rangeHandler{payload: payload, cutAfter: 4 * 1024}
	scanner := &fakeScanner{}
	eng, fs, srv1, mediaRoot := testEngine(t, h1, payload, scanner)

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}

	// First attempt: should fail after the connection drop, but leave the
	// partial file behind for resume.
	err = eng.Download(context.Background(), id)
	if err == nil {
		t.Fatalf("expected first attempt to fail")
	}
	partial := eng.layout.Partial("plex", fs.item.ID)
	st, statErr := os.Stat(partial)
	if statErr != nil {
		t.Fatalf("partial missing after kill: %v", statErr)
	}
	if st.Size() == 0 || st.Size() >= int64(len(payload)) {
		t.Fatalf("unexpected partial size after kill: %d (full=%d)", st.Size(), len(payload))
	}
	partialAfterKill := st.Size()

	// Swap in a second server that serves the FULL payload, and verify the
	// client only requests bytes >= partialAfterKill via the Range header.
	srv1.Close()
	var startObserved int64 = -1
	h2 := &rangeHandler{payload: payload}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rh := r.Header.Get("Range"); rh != "" {
			spec := strings.TrimPrefix(rh, "bytes=")
			parts := strings.SplitN(spec, "-", 2)
			if n, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				atomic.StoreInt64(&startObserved, n)
			}
		}
		h2.ServeHTTP(w, r)
	}))
	defer srv2.Close()
	fs.url = srv2.URL + "/file.mkv"

	// We need to reset the row from 'error' (first attempt marked it) back to
	// queued so Download() will proceed. In real life, the operator would
	// re-queue or the daemon would catch transient errors before they hit
	// markError. Here we exercise the reconciliation path directly.
	if _, err := eng.store.ExecContext(context.Background(),
		`UPDATE items SET status = 'queued', error = NULL WHERE id = ?`, id); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	if err := eng.Download(context.Background(), id); err != nil {
		t.Fatalf("resume download: %v", err)
	}

	if got := atomic.LoadInt64(&startObserved); got != partialAfterKill {
		t.Errorf("resume Range start = %d, want %d (bytes saved by first attempt)", got, partialAfterKill)
	}

	final := filepath.Join(mediaRoot, "movies", "Test (2024).mkv")
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("resumed final file content differs from payload")
	}
}

func TestDownloadIntegrityMismatch(t *testing.T) {
	payload := make([]byte, 1024)
	h := &rangeHandler{payload: payload}
	scanner := &fakeScanner{}
	eng, fs, _, _ := testEngine(t, h, payload, scanner)
	// Claim a larger size than what the server returns; engine must reject.
	fs.item.SizeBytes = 2048

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := eng.Download(context.Background(), id); err == nil {
		t.Fatalf("expected size mismatch error")
	}
	// Row should be marked 'error'.
	var status string
	if err := eng.store.QueryRowContext(context.Background(),
		`SELECT status FROM items WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "error" {
		t.Errorf("status = %q, want %q", status, "error")
	}
	if scanner.calls != 0 {
		t.Errorf("scanner.Refresh called %d times on failure, want 0", scanner.calls)
	}
}

// flakyHandler returns 503 the first `flakeUntil` calls, then serves payload.
type flakyHandler struct {
	inner      *rangeHandler
	flakeUntil int32
	calls      int32
}

func (h *flakyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := atomic.AddInt32(&h.calls, 1)
	if n <= h.flakeUntil {
		http.Error(w, "transient", http.StatusServiceUnavailable)
		return
	}
	h.inner.ServeHTTP(w, r)
}

func TestDownloadRetryOn5xx(t *testing.T) {
	payload := make([]byte, 512)
	h := &flakyHandler{inner: &rangeHandler{payload: payload}, flakeUntil: 2}
	eng, fs, _, mediaRoot := testEngine(t, h, payload, nil)

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := eng.Download(context.Background(), id); err != nil {
		t.Fatalf("download after retries: %v", err)
	}
	if got := atomic.LoadInt32(&h.calls); got < 3 {
		t.Errorf("expected at least 3 server calls, got %d", got)
	}
	final := filepath.Join(mediaRoot, "movies", "Test (2024).mkv")
	if _, err := os.Stat(final); err != nil {
		t.Errorf("final file missing: %v", err)
	}
}

// notFoundHandler always returns 404; engine must abort, not retry.
type notFoundHandler struct{ calls int32 }

func (h *notFoundHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&h.calls, 1)
	http.NotFound(w, r)
}

func TestDownloadAbortOn4xx(t *testing.T) {
	h := &notFoundHandler{}
	eng, fs, _, _ := testEngine(t, h, []byte{0}, nil)

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := eng.Download(context.Background(), id); err == nil {
		t.Fatalf("expected error for 404")
	}
	if got := atomic.LoadInt32(&h.calls); got != 1 {
		t.Errorf("expected exactly 1 call (no retry on 4xx), got %d", got)
	}
}

// noRangeHandler ignores Range headers and always sends the full payload via
// 200 OK. Exercises the engine's "server ignored Range, restart from 0" path.
type noRangeHandler struct {
	payload []byte
	calls   int32
}

func (h *noRangeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&h.calls, 1)
	w.Header().Set("Content-Length", strconv.Itoa(len(h.payload)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, strings.NewReader(string(h.payload)))
}

func TestDownloadRangeIgnoredRestartFromZero(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	h := &noRangeHandler{payload: payload}
	eng, fs, _, mediaRoot := testEngine(t, h, payload, nil)

	// Pre-create a partial file as if a previous attempt had written 100 bytes.
	partial := eng.layout.Partial("plex", fs.item.ID)
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := eng.Download(context.Background(), id); err != nil {
		t.Fatalf("download: %v", err)
	}
	final := filepath.Join(mediaRoot, "movies", "Test (2024).mkv")
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("final differs from payload after Range-ignored restart")
	}
}

func TestRunOnceProcessesQueued(t *testing.T) {
	payload := make([]byte, 64)
	h := &rangeHandler{payload: payload}
	scanner := &fakeScanner{}
	eng, fs, _, mediaRoot := testEngine(t, h, payload, scanner)

	_, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := eng.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}
	final := filepath.Join(mediaRoot, "movies", "Test (2024).mkv")
	if _, err := os.Stat(final); err != nil {
		t.Errorf("final missing after RunOnce: %v", err)
	}
	if scanner.calls != 1 {
		t.Errorf("scanner.calls = %d, want 1", scanner.calls)
	}
}

// TestDownloadTinyBufferStillCompletes guards the configurable copy buffer
// (glb-gdl.14): a buffer far smaller than the payload must still transfer every
// byte across many loop iterations.
func TestDownloadTinyBufferStillCompletes(t *testing.T) {
	payload := make([]byte, 9000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	srv := httptest.NewServer(&rangeHandler{payload: payload})
	t.Cleanup(srv.Close)

	mediaRoot := t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fs := &fakeSource{
		url:  srv.URL + "/file.mkv",
		item: source.Item{ID: "42", Title: "Test", Kind: source.ItemMovie, Year: 2024, Container: "mkv", SizeBytes: int64(len(payload))},
	}
	mgr := storage.NewManager(store, storage.Policy{MediaRoot: mediaRoot})
	eng, err := New(store, mgr, Options{
		MediaRoot: mediaRoot, SourceName: "plex", Resolver: fs,
		BufferSize: 1, // floored to 32 KiB internally, but proves <payload buffers work
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if eng.bufferSize != 32*1024 {
		t.Fatalf("bufferSize = %d, want 32768 (floor)", eng.bufferSize)
	}

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := eng.Download(context.Background(), id); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(mediaRoot, "movies", "Test (2024).mkv"))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("final content differs from payload")
	}
}

// TestQueueResetsErrorRowToQueued guards the retry semantics (glb-gdl.11/.12):
// re-queuing an item whose row is in 'error' must flip it back to 'queued' and
// clear the error, while leaving ready/downloading rows untouched.
func TestQueueResetsErrorRowToQueued(t *testing.T) {
	payload := make([]byte, 16)
	eng, fs, _, _ := testEngine(t, &rangeHandler{payload: payload}, payload, nil)
	ctx := context.Background()

	id, err := eng.Queue(ctx, fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := eng.store.ExecContext(ctx,
		`UPDATE items SET status='error', error='boom' WHERE id=?`, id); err != nil {
		t.Fatalf("seed error: %v", err)
	}

	got, err := eng.Queue(ctx, fs.item)
	if err != nil {
		t.Fatalf("re-queue: %v", err)
	}
	if got != id {
		t.Fatalf("re-queue returned id %d, want existing %d", got, id)
	}
	var status string
	var errMsg *string
	if err := eng.store.QueryRowContext(ctx,
		`SELECT status, error FROM items WHERE id=?`, id).Scan(&status, &errMsg); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
	if errMsg != nil {
		t.Errorf("error = %q, want NULL after requeue", *errMsg)
	}

	// A 'ready' row must NOT be reset by a re-queue.
	if _, err := eng.store.ExecContext(ctx,
		`UPDATE items SET status='ready' WHERE id=?`, id); err != nil {
		t.Fatalf("seed ready: %v", err)
	}
	if _, err := eng.Queue(ctx, fs.item); err != nil {
		t.Fatalf("re-queue ready: %v", err)
	}
	if err := eng.store.QueryRowContext(ctx,
		`SELECT status FROM items WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ready" {
		t.Errorf("status = %q, want ready (re-queue must not disturb a completed item)", status)
	}
}

func TestThroughputMBps(t *testing.T) {
	cases := []struct {
		bytes int64
		d     time.Duration
		want  float64
	}{
		{0, time.Second, 0},
		{1_000_000, 0, 0},
		{1_000_000, time.Second, 1.0},
		{10_000_000, time.Second, 10.0},
		{5_000_000, 2 * time.Second, 2.5},
	}
	for _, c := range cases {
		if got := throughputMBps(c.bytes, c.d); got != c.want {
			t.Errorf("throughputMBps(%d, %v) = %v, want %v", c.bytes, c.d, got, c.want)
		}
	}
}

func TestResetStaleDownloads(t *testing.T) {
	payload := make([]byte, 16)
	h := &rangeHandler{payload: payload}
	eng, fs, _, _ := testEngine(t, h, payload, nil)

	id, err := eng.Queue(context.Background(), fs.item)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	// Simulate a crashed download.
	if _, err := eng.store.ExecContext(context.Background(),
		`UPDATE items SET status = 'downloading' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := eng.ResetStaleDownloads(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var status string
	if err := eng.store.QueryRowContext(context.Background(),
		`SELECT status FROM items WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want %q", status, "queued")
	}
}
