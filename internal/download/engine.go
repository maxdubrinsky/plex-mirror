// Package download is the Phase 2 fetch engine: resumable HTTP Range GET
// against a source.DownloadResolver, atomic rename into a Jellyfin layout,
// and a best-effort scan trigger. State (status, bytes_done, errors) lives
// in the items table so the engine survives process restarts cleanly.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// Scanner is the slice of the Jellyfin admin client the engine needs. Wired as
// an interface so tests can stub it and downloads can run without Jellyfin
// configured at all.
type Scanner interface {
	Refresh(ctx context.Context) error
}

// Options bundles the engine's external dependencies + tunables.
type Options struct {
	MediaRoot   string
	Concurrency int          // default 2
	HTTPClient  *http.Client // default 30s timeout; long-running streaming uses ctx
	Scanner     Scanner      // optional; nil disables post-download scan
	SourceName  string       // identifier stored in items.source (e.g. "plex")
	Resolver    source.DownloadResolver

	// RetryAttempts / RetryBaseDelay control transient error retries. Defaults
	// 5 attempts / 1s base (capped at 30s).
	RetryAttempts  int
	RetryBaseDelay time.Duration

	// ProgressEvery throttles bytes_done flushes to the DB. Default 2s.
	ProgressEvery time.Duration

	// BufferSize is the copy buffer per stream. Default 1 MiB. A larger buffer
	// cuts per-chunk syscall/loop overhead on the high-latency remote links Plex
	// shares typically use; see glb-gdl.14. Floored at 32 KiB.
	BufferSize int
}

// Engine drives one source. Multi-source deployments would run one Engine per
// source — keeping the resolver per-engine sidesteps having to look up the
// right resolver for each row at download time.
type Engine struct {
	store      *db.Store
	storage    *storage.Manager
	layout     Layout
	http       *http.Client
	scanner    Scanner
	resolver   source.DownloadResolver
	sourceName string

	concurrency    int
	retryAttempts  int
	retryBaseDelay time.Duration
	progressEvery  time.Duration
	bufferSize     int

	// rand is used to jitter backoff so a thundering herd of clients (us +
	// other Plex shares) don't all retry in lockstep.
	rand *rand.Rand
	mu   sync.Mutex // guards rand
}

// New constructs an Engine. Resolver must be non-nil — there's nothing to
// download without one. Scanner may be nil; downloads still complete, we just
// skip the post-import refresh.
func New(store *db.Store, storageMgr *storage.Manager, opts Options) (*Engine, error) {
	if store == nil {
		return nil, errors.New("download: store is required")
	}
	if storageMgr == nil {
		return nil, errors.New("download: storage manager is required")
	}
	if opts.Resolver == nil {
		return nil, errors.New("download: resolver is required")
	}
	if opts.MediaRoot == "" {
		return nil, errors.New("download: MediaRoot is required")
	}
	if strings.TrimSpace(opts.SourceName) == "" {
		return nil, errors.New("download: SourceName is required")
	}

	conc := opts.Concurrency
	if conc <= 0 {
		conc = 2
	}
	hc := opts.HTTPClient
	if hc == nil {
		// No Timeout: a multi-GB download under a 30s deadline would always fail.
		// Per-request progress + ctx cancellation are how we bound transfer time.
		// Tune the transport for large sequential media transfers (glb-gdl.14):
		// bigger socket read/write buffers cut TLS-record/syscall overhead, and
		// disabling transparent compression skips a pointless gzip negotiation on
		// already-compressed video. Keep-alive (transport reuse) means resumes hit
		// a warm connection rather than re-dialing + re-TLS each attempt.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ReadBufferSize = 256 * 1024
		tr.WriteBufferSize = 256 * 1024
		tr.DisableCompression = true
		tr.MaxIdleConnsPerHost = 4
		hc = &http.Client{Transport: tr}
	}
	attempts := opts.RetryAttempts
	if attempts <= 0 {
		attempts = 5
	}
	base := opts.RetryBaseDelay
	if base <= 0 {
		base = time.Second
	}
	prog := opts.ProgressEvery
	if prog <= 0 {
		prog = 2 * time.Second
	}
	bufSize := opts.BufferSize
	if bufSize <= 0 {
		bufSize = 1 << 20 // 1 MiB
	}
	if bufSize < 32*1024 {
		bufSize = 32 * 1024
	}

	return &Engine{
		store:          store,
		storage:        storageMgr,
		layout:         Layout{MediaRoot: opts.MediaRoot},
		http:           hc,
		scanner:        opts.Scanner,
		resolver:       opts.Resolver,
		sourceName:     opts.SourceName,
		concurrency:    conc,
		retryAttempts:  attempts,
		retryBaseDelay: base,
		progressEvery:  prog,
		bufferSize:     bufSize,
		rand:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

// Queue idempotently inserts (or upserts) an item row in 'queued' state and
// returns its local id. If the row is already 'ready' or 'downloading', the
// existing id is returned and no state is changed (the caller decides whether
// to force a re-download by deleting the row first). A previously 'error' row
// is reset to 'queued' (clearing the error + bumping queued_at) so re-queuing —
// the portal "Retry" button and the bulk season/show queue (glb-gdl.11/.12) —
// genuinely retries instead of leaving the row stranded in 'error', which no
// worker scans.
func (e *Engine) Queue(ctx context.Context, item source.Item) (int64, error) {
	if item.ID == "" {
		return 0, errors.New("download: item ID is required to queue")
	}
	// Validate layout up front so we don't queue rows we can't ever land.
	if _, err := e.layout.Final(e.sourceName, item); err != nil {
		return 0, fmt.Errorf("download: layout validation: %w", err)
	}

	tx, err := e.store.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("queue begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Try insert first; on conflict pull existing id without rewriting state.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO items (source, source_key, title, container, size_bytes, status, queued_at)
		VALUES (?, ?, ?, ?, ?, 'queued', unixepoch())
		ON CONFLICT(source, source_key) DO UPDATE SET
			title      = excluded.title,
			container  = excluded.container,
			size_bytes = excluded.size_bytes,
			status     = CASE WHEN status = 'error' THEN 'queued'     ELSE status    END,
			error      = CASE WHEN status = 'error' THEN NULL          ELSE error     END,
			queued_at  = CASE WHEN status = 'error' THEN unixepoch()   ELSE queued_at END
	`, e.sourceName, item.ID, item.Title, item.Container, item.SizeBytes)
	if err != nil {
		return 0, fmt.Errorf("queue upsert: %w", err)
	}

	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM items WHERE source = ? AND source_key = ?`,
		e.sourceName, item.ID,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("queue lookup id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("queue commit: %w", err)
	}
	return id, nil
}

// itemRow is the slice of the items table the engine cares about during a
// download. Centralized so the SELECT and Scan list stay in sync.
type itemRow struct {
	id        int64
	source    string
	sourceKey string
	title     string
	container string
	sizeBytes int64
	bytesDone int64
	status    string
}

// loadRow reads one row by id. Returns sql.ErrNoRows wrapped.
func (e *Engine) loadRow(ctx context.Context, id int64) (itemRow, error) {
	var r itemRow
	err := e.store.QueryRowContext(ctx, `
		SELECT id, source, source_key, title, COALESCE(container, ''),
		       COALESCE(size_bytes, 0), bytes_done, status
		  FROM items WHERE id = ?
	`, id).Scan(&r.id, &r.source, &r.sourceKey, &r.title, &r.container,
		&r.sizeBytes, &r.bytesDone, &r.status)
	if err != nil {
		return itemRow{}, fmt.Errorf("load item %d: %w", id, err)
	}
	return r, nil
}

// Download fetches a single queued item to completion. Safe to call after a
// crash mid-download — it picks up from the partial file and resumes via
// Range. Idempotent against 'ready' rows (returns nil without touching disk).
func (e *Engine) Download(ctx context.Context, id int64) error {
	row, err := e.loadRow(ctx, id)
	if err != nil {
		return err
	}
	if row.source != e.sourceName {
		return fmt.Errorf("download: row %d belongs to source %q, engine drives %q",
			id, row.source, e.sourceName)
	}
	if row.status == "ready" {
		return nil
	}

	// Resolve a fresh URL every attempt — Plex tokens can rotate and
	// short-lived signed URLs (some Plex configurations) shouldn't be reused
	// across long resumes.
	target, err := e.resolver.ResolveDownloadURL(ctx, row.sourceKey)
	if err != nil {
		_ = e.markError(ctx, id, fmt.Sprintf("resolve url: %v", err))
		return fmt.Errorf("download: resolve url for %q: %w", row.sourceKey, err)
	}

	// Restore the canonical Item shape so the layout module gets the show /
	// season / episode fields. The Item.Kind needs to come from the source —
	// re-fetching metadata feels heavy here, so we ask the resolver for it
	// only if it implements an optional getter; otherwise fall back to the
	// fields we have on row.
	item := source.Item{
		ID:        row.sourceKey,
		Title:     row.title,
		Container: row.container,
		SizeBytes: row.sizeBytes,
	}
	// For Phase 2 the CLI seeds Queue() with the canonical Item, and Queue()
	// validates the layout — so the row already has enough to land somewhere
	// once we re-fetch metadata. To keep Download self-contained without a
	// schema bump, we re-fetch metadata via the Source if the resolver is
	// also a source.Source (the Plex adapter is). This keeps the items table
	// narrow while still letting Download() work standalone.
	if src, ok := e.resolver.(source.Source); ok {
		if fresh, mErr := src.GetMetadata(ctx, row.sourceKey); mErr == nil {
			item = fresh
		}
	}
	finalPath, err := e.layout.Final(e.sourceName, item)
	if err != nil {
		_ = e.markError(ctx, id, fmt.Sprintf("layout: %v", err))
		return fmt.Errorf("download: layout for %q: %w", row.sourceKey, err)
	}
	partialPath := e.layout.Partial(e.sourceName, row.sourceKey)

	if err := os.MkdirAll(filepath.Dir(partialPath), 0o755); err != nil {
		return fmt.Errorf("download: mkdir partials: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("download: mkdir final: %w", err)
	}

	// Reconcile DB bytes_done with what's actually on disk. Filesystem is truth:
	// a half-written partial with 100MB on disk and bytes_done=200MB in the DB
	// would have us skipping 100MB of bytes we never received.
	startAt := int64(0)
	if st, statErr := os.Stat(partialPath); statErr == nil {
		startAt = st.Size()
	}
	if startAt != row.bytesDone {
		slog.Info("download: reconciled bytes_done from disk",
			"id", id, "db", row.bytesDone, "disk", startAt)
		row.bytesDone = startAt
	}

	if err := e.markDownloading(ctx, id, startAt); err != nil {
		return err
	}

	// Throughput accounting (glb-gdl.14): measure only the bytes this run pulls
	// over the wire (a resume that starts at 90% shouldn't report a fake 10x).
	bytesAtStart := startAt
	transferStart := time.Now()

	// Retry loop. Each attempt either makes progress (returns nil), is
	// retryable (returns retryableError), or is permanent (returns other).
	var lastErr error
	for attempt := 0; attempt < e.retryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := e.fetchOnce(ctx, id, target, partialPath, &startAt, row.sizeBytes)
		if err == nil {
			lastErr = nil // clear any earlier transient error now that we've succeeded
			break
		}
		lastErr = err

		var perm permanentError
		if errors.As(err, &perm) {
			_ = e.markError(ctx, id, perm.Error())
			return err
		}
		// Transient: back off and retry.
		delay := e.backoff(attempt)
		slog.Warn("download: transient failure, retrying",
			"id", id, "attempt", attempt+1, "delay", delay, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		// Re-stat partial; previous attempt may have made partial progress
		// even though it errored.
		if st, statErr := os.Stat(partialPath); statErr == nil {
			startAt = st.Size()
		}
	}
	if lastErr != nil {
		_ = e.markError(ctx, id, fmt.Sprintf("retries exhausted: %v", lastErr))
		return fmt.Errorf("download: retries exhausted: %w", lastErr)
	}

	// Integrity: final partial size should match the declared item size when
	// we have one. Bare zero means upstream didn't tell us; trust the transfer.
	finalSize, err := fileSize(partialPath)
	if err != nil {
		_ = e.markError(ctx, id, fmt.Sprintf("stat partial: %v", err))
		return err
	}
	if row.sizeBytes > 0 && finalSize != row.sizeBytes {
		msg := fmt.Sprintf("size mismatch: got %d expected %d", finalSize, row.sizeBytes)
		_ = e.markError(ctx, id, msg)
		return fmt.Errorf("download: %s", msg)
	}

	// Atomic move. Same filesystem (both under MediaRoot) → rename is atomic
	// and Jellyfin will never see a partial file under shows/ or movies/.
	if err := os.Rename(partialPath, finalPath); err != nil {
		_ = e.markError(ctx, id, fmt.Sprintf("rename: %v", err))
		return fmt.Errorf("download: rename: %w", err)
	}

	if _, err := e.storage.RecordReady(ctx, storage.ReadyItem{
		Source:    e.sourceName,
		SourceKey: row.sourceKey,
		Title:     item.Title,
		Container: item.Container,
		LocalPath: finalPath,
		SizeBytes: finalSize,
	}); err != nil {
		return fmt.Errorf("download: record ready: %w", err)
	}

	if e.scanner != nil {
		// Best-effort: a scan failure shouldn't undo a successful download.
		if err := e.scanner.Refresh(ctx); err != nil {
			slog.Warn("download: scan trigger failed", "id", id, "err", err)
		}
	}

	transferred := finalSize - bytesAtStart
	elapsed := time.Since(transferStart)
	slog.Info("download: complete",
		"id", id, "path", finalPath, "bytes", finalSize,
		"transferred_bytes", transferred,
		"elapsed", elapsed.Round(time.Millisecond).String(),
		"throughput_mbps", throughputMBps(transferred, elapsed))
	return nil
}

// throughputMBps reports the effective transfer rate in MB/s (decimal MB, the
// unit users expect from speedtests), rounded to one decimal. Returns 0 for a
// zero/negative byte count or an immeasurably short interval.
func throughputMBps(bytes int64, d time.Duration) float64 {
	if bytes <= 0 || d <= 0 {
		return 0
	}
	mbps := float64(bytes) / 1e6 / d.Seconds()
	return float64(int64(mbps*10+0.5)) / 10
}

// fetchOnce makes one HTTP attempt. startAt is read+updated as bytes are
// written. expectedSize (0 if unknown) is used to set the Range upper bound /
// validate Content-Range.
func (e *Engine) fetchOnce(
	ctx context.Context,
	id int64,
	target *source.DownloadTarget,
	partialPath string,
	startAt *int64,
	expectedSize int64,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		return permanent("build request: " + err.Error())
	}
	for k, vv := range target.Headers {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	if *startAt > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", *startAt))
	}

	resp, err := e.http.Do(req)
	if err != nil {
		// Network / DNS / dial errors are retryable by definition.
		return fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// 200 means server ignored our Range and is sending the whole file.
		// Truncate any existing partial so we don't end up with bytes_done
		// pointing past the start of what we're about to write.
		if *startAt > 0 {
			slog.Info("download: server ignored Range, restarting from byte 0", "id", id)
			if err := os.Remove(partialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("truncate partial: %w", err)
			}
			*startAt = 0
		}
	case http.StatusPartialContent:
		// Expected resume case. Verify Content-Range matches our expectation.
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if total, ok := parseContentRangeTotal(cr); ok && expectedSize > 0 && total != expectedSize {
				return permanent(fmt.Sprintf("content-range total %d != expected %d", total, expectedSize))
			}
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// We asked for bytes past the file's end. Either we already have the
		// whole file (validate via expectedSize) or the upstream truncated.
		st, statErr := os.Stat(partialPath)
		if statErr != nil {
			return permanent(fmt.Sprintf("416 with no partial file: %v", statErr))
		}
		if expectedSize > 0 && st.Size() != expectedSize {
			return permanent(fmt.Sprintf("416 with size %d != expected %d", st.Size(), expectedSize))
		}
		// Partial is already at the expected size; signal "no more bytes to read".
		return nil
	default:
		// 4xx (other than 416): auth lost, item gone, etc. Permanent.
		// 5xx: server hiccup, retryable.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return permanent(fmt.Sprintf("http %s", resp.Status))
		}
		return fmt.Errorf("http %s", resp.Status)
	}

	// Open partial for append (or create on first attempt).
	flag := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if *startAt == 0 {
		// Make sure we don't accidentally append to leftover bytes when the
		// caller reconciled startAt to 0 (truncate path above does this too,
		// but be defensive).
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(partialPath, flag, 0o644)
	if err != nil {
		return fmt.Errorf("open partial: %w", err)
	}
	defer f.Close()

	buf := make([]byte, e.bufferSize)
	lastFlush := time.Now()
	for {
		// Allow ctx cancellation between reads — body.Read will also unblock
		// when the request's ctx is cancelled, but this gives us a tighter
		// loop for the common "ctx cancelled during a slow chunk" case.
		if err := ctx.Err(); err != nil {
			_ = e.flushProgress(context.Background(), id, *startAt)
			return err
		}

		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return fmt.Errorf("write partial: %w", werr)
			}
			*startAt += int64(n)
			if time.Since(lastFlush) >= e.progressEvery {
				_ = e.flushProgress(ctx, id, *startAt)
				lastFlush = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// Mid-transfer error: flush progress so resume picks up correctly.
			_ = e.flushProgress(context.Background(), id, *startAt)
			return fmt.Errorf("read body: %w", rerr)
		}
	}
	// Final flush so the DB reflects the on-disk size before we commit to
	// renaming.
	_ = e.flushProgress(ctx, id, *startAt)
	return f.Sync()
}

// permanentError is returned by fetchOnce for failures the retry loop must
// not retry (auth, 4xx, integrity violations on partial).
type permanentError struct{ msg string }

func (e permanentError) Error() string { return e.msg }
func permanent(msg string) error       { return permanentError{msg: msg} }

func (e *Engine) backoff(attempt int) time.Duration {
	// Exponential: base * 2^attempt, capped at 30s, with up to ±25% jitter.
	d := min(e.retryBaseDelay<<attempt, 30*time.Second)
	e.mu.Lock()
	jitter := time.Duration(float64(d) * (e.rand.Float64()*0.5 - 0.25))
	e.mu.Unlock()
	return d + jitter
}

func (e *Engine) markDownloading(ctx context.Context, id int64, bytesDone int64) error {
	_, err := e.store.ExecContext(ctx, `
		UPDATE items SET status = 'downloading',
		       started_at = COALESCE(started_at, unixepoch()),
		       bytes_done = ?,
		       last_progress_at = unixepoch(),
		       error = NULL
		 WHERE id = ?
	`, bytesDone, id)
	if err != nil {
		return fmt.Errorf("mark downloading id=%d: %w", id, err)
	}
	return nil
}

func (e *Engine) flushProgress(ctx context.Context, id, bytesDone int64) error {
	_, err := e.store.ExecContext(ctx, `
		UPDATE items SET bytes_done = ?, last_progress_at = unixepoch()
		 WHERE id = ?
	`, bytesDone, id)
	return err
}

func (e *Engine) markError(ctx context.Context, id int64, msg string) error {
	_, err := e.store.ExecContext(ctx, `
		UPDATE items SET status = 'error', error = ? WHERE id = ?
	`, msg, id)
	return err
}

// ResetStaleDownloads flips any rows still 'downloading' back to 'queued'.
// Run at engine startup so a crash mid-download doesn't strand items in a
// state no worker will pick up.
func (e *Engine) ResetStaleDownloads(ctx context.Context) error {
	res, err := e.store.ExecContext(ctx, `
		UPDATE items SET status = 'queued'
		 WHERE source = ? AND status = 'downloading'
	`, e.sourceName)
	if err != nil {
		return fmt.Errorf("reset stale downloads: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Info("download: reset stale rows to queued", "count", n, "source", e.sourceName)
	}
	return nil
}

// RunOnce drains all currently-queued items concurrently up to e.concurrency,
// returning once every dispatched download has settled. Errors are logged per
// item; the returned error is non-nil only when the *queue scan* itself
// fails. Used by the CLI for `plex-mirror download --item=ID` to keep blocking
// semantics simple, and by tests.
func (e *Engine) RunOnce(ctx context.Context) error {
	rows, err := e.store.QueryContext(ctx,
		`SELECT id FROM items WHERE source = ? AND status = 'queued' ORDER BY queued_at ASC, id ASC`,
		e.sourceName)
	if err != nil {
		return fmt.Errorf("scan queued: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan queued row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	sem := make(chan struct{}, e.concurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := e.Download(ctx, id); err != nil {
				slog.Warn("download: item failed", "id", id, "err", err)
			}
		}(id)
	}
	wg.Wait()
	return nil
}

// Run loops RunOnce on a poll interval until ctx is cancelled. A zero
// interval disables daemon mode (returns immediately). Intended for service
// mode wired up alongside the HTTP server.
func (e *Engine) Run(ctx context.Context, every time.Duration) {
	// Reset stale rows first, before the disabled-daemon early return: a crash or
	// a reload can leave rows in 'downloading' that must be reclaimed even if the
	// poll loop is turned off this generation.
	if err := e.ResetStaleDownloads(ctx); err != nil {
		slog.Warn("download: ResetStaleDownloads failed", "err", err)
	}
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := e.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("download: RunOnce failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// parseContentRangeTotal pulls the total size out of a "bytes A-B/TOTAL"
// header. Returns (0, false) when the header is malformed or uses "*" for
// the total (RFC 7233 allows it).
func parseContentRangeTotal(h string) (int64, bool) {
	// Expected: "bytes 0-499/1234"
	parts := strings.SplitN(h, "/", 2)
	if len(parts) != 2 {
		return 0, false
	}
	if parts[1] == "*" {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return st.Size(), nil
}
