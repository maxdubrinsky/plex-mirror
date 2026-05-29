package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// MirrorItem is the local-DB view of an item row: its mirror lifecycle state,
// download progress, and where it landed. Distinct from source.Item (the remote
// catalog descriptor) — this is what we know about our copy.
type MirrorItem struct {
	ID          int64   `json:"id"`
	Source      string  `json:"source"`
	SourceKey   string  `json:"source_key"`
	Title       string  `json:"title"`
	Container   string  `json:"container,omitempty"`
	SizeBytes   int64   `json:"size_bytes"`
	BytesDone   int64   `json:"bytes_done"`
	Status      string  `json:"status"`
	LocalPath   string  `json:"local_path,omitempty"`
	Error       string  `json:"error,omitempty"`
	Progress    float64 `json:"progress"` // 0..1; bytes_done/size_bytes, 0 when size unknown
	QueuedAt    int64   `json:"queued_at,omitempty"`
	StartedAt   int64   `json:"started_at,omitempty"`
	CompletedAt int64   `json:"completed_at,omitempty"`
}

// StorageStats is the storage view: current usage vs. configured caps and the
// filesystem headroom under the media root.
type StorageStats struct {
	MediaRoot    string `json:"media_root"`
	UsedBytes    int64  `json:"used_bytes"`
	FreeBytes    int64  `json:"free_bytes"`
	HardCapBytes int64  `json:"hard_cap_bytes"` // 0 = uncapped
	SoftCapBytes int64  `json:"soft_cap_bytes"` // 0 = unset
	ItemsReady   int    `json:"items_ready"`
}

// itemColumns is the shared SELECT list for MirrorItem scans. Kept in one place
// so the column order and scanItem stay in lockstep.
const itemColumns = `id, source, source_key, title, COALESCE(container, ''),
	COALESCE(size_bytes, 0), bytes_done, status, COALESCE(local_path, ''),
	COALESCE(error, ''), COALESCE(queued_at, 0), COALESCE(started_at, 0),
	COALESCE(completed_at, 0)`

func scanItem(sc interface{ Scan(...any) error }) (MirrorItem, error) {
	var it MirrorItem
	if err := sc.Scan(
		&it.ID, &it.Source, &it.SourceKey, &it.Title, &it.Container,
		&it.SizeBytes, &it.BytesDone, &it.Status, &it.LocalPath,
		&it.Error, &it.QueuedAt, &it.StartedAt, &it.CompletedAt,
	); err != nil {
		return MirrorItem{}, err
	}
	if it.SizeBytes > 0 {
		it.Progress = float64(it.BytesDone) / float64(it.SizeBytes)
	}
	return it, nil
}

// QueueDownload resolves the item's metadata on the source, then idempotently
// enqueues it. The running download daemon (Service.Start) picks it up; callers
// poll DownloadStatus for progress. Only Plex is downloadable today.
//
// Returns the resulting MirrorItem (reflecting the row's real status, which may
// already be 'ready'/'downloading' if it was queued before).
func (s *Service) QueueDownload(ctx context.Context, sourceName, itemID string) (MirrorItem, error) {
	rt := s.now()
	if rt.engine == nil {
		return MirrorItem{}, ErrDownloadUnavailable
	}
	src, err := rt.source(sourceName)
	if err != nil {
		return MirrorItem{}, err
	}
	if _, ok := src.(source.DownloadResolver); !ok {
		return MirrorItem{}, fmt.Errorf("%w: source %q has no download URLs", source.ErrUnsupported, sourceName)
	}

	item, err := src.GetMetadata(ctx, itemID)
	if err != nil {
		return MirrorItem{}, err
	}

	id, err := rt.engine.Queue(ctx, item)
	if err != nil {
		return MirrorItem{}, fmt.Errorf("queue: %w", err)
	}
	return s.itemByID(ctx, id)
}

// DownloadStatus reports one item (when id != nil) or all in-flight items
// (queued / downloading / error) when id is nil. A nil id never includes ready
// or evicted rows — use ListMirrored for the settled inventory.
func (s *Service) DownloadStatus(ctx context.Context, id *int64) ([]MirrorItem, error) {
	if id != nil {
		it, err := s.itemByID(ctx, *id)
		if err != nil {
			return nil, err
		}
		return []MirrorItem{it}, nil
	}

	rows, err := s.store.QueryContext(ctx,
		`SELECT `+itemColumns+` FROM items
		  WHERE status IN ('queued', 'downloading', 'error')
		  ORDER BY queued_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query in-flight: %w", err)
	}
	defer rows.Close()
	return collectItems(rows)
}

// ListMirrored returns the ready (locally available) inventory, newest first.
// filter, when non-empty, keeps only titles containing it (case-insensitive).
func (s *Service) ListMirrored(ctx context.Context, filter string) ([]MirrorItem, error) {
	q := `SELECT ` + itemColumns + ` FROM items WHERE status = 'ready'`
	var args []any
	if filter != "" {
		q += ` AND instr(lower(title), lower(?)) > 0`
		args = append(args, filter)
	}
	q += ` ORDER BY completed_at DESC, id DESC`

	rows, err := s.store.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query mirrored: %w", err)
	}
	defer rows.Close()
	return collectItems(rows)
}

// StorageStats returns current usage, free space, configured caps, and the
// count of ready items.
func (s *Service) StorageStats(ctx context.Context) (StorageStats, error) {
	rt := s.now()
	used, err := rt.storage.UsedBytes(ctx)
	if err != nil {
		return StorageStats{}, err
	}
	free, err := rt.storage.FreeBytesAt(rt.cfg.MediaRoot)
	if err != nil {
		return StorageStats{}, err
	}

	var ready int
	if err := s.store.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM items WHERE status = 'ready'`).Scan(&ready); err != nil {
		return StorageStats{}, fmt.Errorf("count ready: %w", err)
	}

	return StorageStats{
		MediaRoot:    rt.cfg.MediaRoot,
		UsedBytes:    used,
		FreeBytes:    free,
		HardCapBytes: rt.cfg.StorageHardCapBytes,
		SoftCapBytes: rt.cfg.StorageSoftCapBytes,
		ItemsReady:   ready,
	}, nil
}

// Evict manually removes one item by local id (deletes the file, flips the row
// to 'evicted'). Returns ErrItemNotFound if no such row.
func (s *Service) Evict(ctx context.Context, id int64) (storage.EvictedItem, error) {
	ev, err := s.now().storage.EvictItem(ctx, id)
	if errors.Is(err, storage.ErrItemNotFound) {
		return storage.EvictedItem{}, ErrItemNotFound
	}
	return ev, err
}

// itemByID loads a single MirrorItem, mapping a missing row to ErrItemNotFound.
func (s *Service) itemByID(ctx context.Context, id int64) (MirrorItem, error) {
	row := s.store.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorItem{}, ErrItemNotFound
	}
	if err != nil {
		return MirrorItem{}, fmt.Errorf("load item %d: %w", id, err)
	}
	return it, nil
}

func collectItems(rows *sql.Rows) ([]MirrorItem, error) {
	items := []MirrorItem{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate items: %w", err)
	}
	return items, nil
}
