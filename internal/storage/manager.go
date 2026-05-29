package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/db"
)

// ErrItemNotFound is returned by EvictItem when no row matches the given id.
var ErrItemNotFound = errors.New("storage: item not found")

type Policy struct {
	MediaRoot    string
	HardCapBytes int64
	SoftCapBytes int64
}

type Manager struct {
	store  *db.Store
	policy Policy
}

type ReadyItem struct {
	Source    string
	SourceKey string
	Title     string
	Container string
	LocalPath string
	SizeBytes int64
}

type EvictedItem struct {
	ID        int64  `json:"id"`
	LocalPath string `json:"local_path"`
	SizeBytes int64  `json:"size_bytes"`
	Reason    string `json:"reason"`
}

type Report struct {
	ScannedItems    int           `json:"scanned_items"`
	UsedBytesBefore int64         `json:"used_bytes_before"`
	UsedBytesAfter  int64         `json:"used_bytes_after"`
	EvictedItems    []EvictedItem `json:"evicted_items"`
	DryRun          bool          `json:"dry_run"`
}

func NewManager(store *db.Store, policy Policy) *Manager {
	return &Manager{store: store, policy: policy}
}

// RecordReady upserts an item as ready after a successful download. The
// UNIQUE(source, source_key) index drives the dedup so a re-download of the
// same upstream key cleanly overwrites local_path/size_bytes and bumps the
// row back to 'ready' even if it had been marked evicted.
func (m *Manager) RecordReady(ctx context.Context, item ReadyItem) (int64, error) {
	if item.Source == "" || item.SourceKey == "" {
		return 0, errors.New("storage: RecordReady requires source and source_key")
	}
	if item.LocalPath == "" {
		return 0, errors.New("storage: RecordReady requires local_path")
	}

	tx, err := m.store.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO items (source, source_key, title, container, size_bytes, local_path,
			status, bytes_done, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, 'ready', ?, unixepoch())
		ON CONFLICT(source, source_key) DO UPDATE SET
			title        = excluded.title,
			container    = excluded.container,
			size_bytes   = excluded.size_bytes,
			local_path   = excluded.local_path,
			status       = 'ready',
			bytes_done   = excluded.bytes_done,
			error        = NULL,
			completed_at = unixepoch()
	`, item.Source, item.SourceKey, item.Title, item.Container, item.SizeBytes,
		item.LocalPath, item.SizeBytes)
	if err != nil {
		return 0, fmt.Errorf("upsert item: %w", err)
	}

	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM items WHERE source = ? AND source_key = ?`,
		item.Source, item.SourceKey,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup item id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

func (m *Manager) MarkAccessed(ctx context.Context, itemID int64) error {
	_, err := m.store.ExecContext(ctx,
		`UPDATE items SET last_accessed = unixepoch() WHERE id = ?`, itemID)
	if err != nil {
		return fmt.Errorf("mark accessed: %w", err)
	}
	return nil
}

func (m *Manager) UsedBytes(ctx context.Context) (int64, error) {
	var used sql.NullInt64
	err := m.store.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0) FROM items WHERE status = 'ready'`,
	).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("sum used bytes: %w", err)
	}
	return used.Int64, nil
}

// FreeBytesAt reports filesystem free space at path. Wraps syscall.Statfs so
// the caller doesn't need to import syscall on platforms where the struct
// fields differ.
func (m *Manager) FreeBytesAt(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Bavail * Bsize is the unprivileged-user free space, which is what we
	// care about for eviction headroom.
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// EvictItem manually evicts a single item by id: removes its local file (guarded
// to MediaRoot so this can never become a delete-anywhere primitive) and flips
// the row to 'evicted'. Returns ErrItemNotFound if no such row. Idempotent
// against an already-evicted row (local_path is NULL → nothing to remove).
func (m *Manager) EvictItem(ctx context.Context, id int64) (EvictedItem, error) {
	var localPath sql.NullString
	var size sql.NullInt64
	var status string
	err := m.store.QueryRowContext(ctx,
		`SELECT local_path, COALESCE(size_bytes, 0), status FROM items WHERE id = ?`, id,
	).Scan(&localPath, &size, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return EvictedItem{}, ErrItemNotFound
	}
	if err != nil {
		return EvictedItem{}, fmt.Errorf("load item %d: %w", id, err)
	}

	path := ""
	if localPath.Valid {
		path = localPath.String
	}
	if path != "" {
		safe, escapeErr := pathIsUnderRoot(m.policy.MediaRoot, path)
		if escapeErr != nil || !safe {
			return EvictedItem{}, fmt.Errorf(
				"storage: refusing to evict path outside media root (id=%d path=%q root=%q): %w",
				id, path, m.policy.MediaRoot, escapeErr)
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			// Log and continue: we still flip the row so DB and disk converge on
			// "gone" rather than leaving a ready row pointing at a file we tried
			// to remove.
			slog.Warn("storage: remove file failed", "id", id, "path", path, "err", rmErr)
		}
	}

	if _, err := m.store.ExecContext(ctx,
		`UPDATE items SET status = 'evicted', local_path = NULL WHERE id = ?`, id,
	); err != nil {
		return EvictedItem{}, fmt.Errorf("mark evicted id=%d: %w", id, err)
	}

	return EvictedItem{ID: id, LocalPath: path, SizeBytes: size.Int64, Reason: "manual"}, nil
}

func (m *Manager) EvictNow(ctx context.Context, dryRun bool) (Report, error) {
	report := Report{DryRun: dryRun, EvictedItems: []EvictedItem{}}

	used, err := m.UsedBytes(ctx)
	if err != nil {
		return report, err
	}
	report.UsedBytesBefore = used
	report.UsedBytesAfter = used

	target, reason, evict := m.evictionTarget(used)
	if !evict {
		// No eviction needed; still report scanned count = 0 since we did not iterate.
		return report, nil
	}

	// TODO(LRU): once Jellyfin played-state lands, switch ordering to
	// `ORDER BY COALESCE(last_accessed, completed_at) ASC, id ASC` so genuinely
	// stale items go before merely old ones. For now age-only is deterministic
	// and matches the Phase 3 ticket.
	rows, err := m.store.QueryContext(ctx, `
		SELECT id, local_path, COALESCE(size_bytes, 0)
		FROM items
		WHERE status = 'ready'
		ORDER BY COALESCE(completed_at, 0) ASC, id ASC
	`)
	if err != nil {
		return report, fmt.Errorf("query ready items: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id        int64
		localPath sql.NullString
		size      int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.localPath, &c.size); err != nil {
			return report, fmt.Errorf("scan ready item: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("iterate ready items: %w", err)
	}
	report.ScannedItems = len(candidates)

	// Prepared statement for the hot loop; one per EvictNow call.
	var evictStmt *sql.Stmt
	if !dryRun {
		evictStmt, err = m.store.PrepareContext(ctx,
			`UPDATE items SET status = 'evicted', local_path = NULL WHERE id = ?`)
		if err != nil {
			return report, fmt.Errorf("prepare evict stmt: %w", err)
		}
		defer evictStmt.Close()
	}

	running := used
	for _, c := range candidates {
		if running <= target {
			break
		}

		path := ""
		if c.localPath.Valid {
			path = c.localPath.String
		}

		// Path-escape guard. A bug elsewhere (or a hand-tampered DB) could
		// stuff /etc/passwd into local_path. We refuse to remove anything
		// that doesn't resolve under MediaRoot so the storage manager can
		// never be turned into a delete-anywhere primitive.
		safe, escapeErr := pathIsUnderRoot(m.policy.MediaRoot, path)
		if escapeErr != nil || !safe {
			slog.Warn("storage: refusing to evict path outside media root",
				"id", c.id, "local_path", path, "media_root", m.policy.MediaRoot,
				"err", escapeErr)
			continue
		}

		if !dryRun {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				// We log and continue so DB state and disk state don't drift:
				// the row will still be flipped to 'evicted' below.
				slog.Warn("storage: remove file failed", "id", c.id, "path", path, "err", err)
			}
			if _, err := evictStmt.ExecContext(ctx, c.id); err != nil {
				return report, fmt.Errorf("mark evicted id=%d: %w", c.id, err)
			}
		}

		report.EvictedItems = append(report.EvictedItems, EvictedItem{
			ID:        c.id,
			LocalPath: path,
			SizeBytes: c.size,
			Reason:    reason,
		})
		running -= c.size
	}

	report.UsedBytesAfter = running
	return report, nil
}

// evictionTarget returns the byte target we want usage to drop below, the
// reason string to stamp on evicted items, and whether eviction should run.
func (m *Manager) evictionTarget(used int64) (int64, string, bool) {
	hard := m.policy.HardCapBytes
	soft := m.policy.SoftCapBytes

	if hard > 0 && used > hard {
		target := soft
		if target <= 0 {
			// Without a soft cap, fall back to 90% of the hard cap so a
			// single eviction pass leaves headroom for the next download.
			target = (hard * 9) / 10
		}
		return target, "over_hard_cap", true
	}
	if soft > 0 && used > soft {
		return soft, "over_soft_cap", true
	}
	return 0, "", false
}

// pathIsUnderRoot reports whether candidate is under root. Both are cleaned
// first; a candidate of "" or a root of "" is rejected.
func pathIsUnderRoot(root, candidate string) (bool, error) {
	if root == "" || candidate == "" {
		return false, errors.New("empty root or candidate")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	absCand, err := filepath.Abs(candidate)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absRoot, absCand)
	if err != nil {
		return false, err
	}
	if rel == "." || rel == "" {
		// The media root itself is not a valid item path.
		return false, nil
	}
	// Anything that has to climb out of root via ".." is rejected.
	if rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return false, nil
	}
	return true, nil
}

// RunSweeper periodically triggers EvictNow when the soft cap is breached. A
// zero interval disables it. Stops cleanly on ctx.Done().
func (m *Manager) RunSweeper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			used, err := m.UsedBytes(ctx)
			if err != nil {
				slog.Warn("storage: sweeper used-bytes failed", "err", err)
				continue
			}
			_, _, evict := m.evictionTarget(used)
			if !evict {
				slog.Debug("storage: sweeper nothing to do", "used_bytes", used)
				continue
			}
			report, err := m.EvictNow(ctx, false)
			if err != nil {
				slog.Warn("storage: sweeper eviction failed", "err", err)
				continue
			}
			slog.Info("storage: sweeper evicted",
				"used_before", report.UsedBytesBefore,
				"used_after", report.UsedBytesAfter,
				"evicted", len(report.EvictedItems))
		}
	}
}
