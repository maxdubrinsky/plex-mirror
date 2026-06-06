package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// maxChildDepth bounds the container descent (show → season → episode is depth
// 2; the guard stops a malformed/cyclic hierarchy from looping forever).
const maxChildDepth = 3

// childPageSize pages ListChildren so "queue the whole thing" never silently
// truncates at a backend's default page size.
const childPageSize = 200

// BulkQueueResult summarizes a "queue this whole container" operation
// (glb-gdl.11 season, glb-gdl.12 show). It's intentionally idempotent: leaves
// already ready/downloading are counted as Skipped and left untouched.
type BulkQueueResult struct {
	Container  string   `json:"container"`        // title of the container queued
	Queued     int      `json:"queued"`           // leaves now in 'queued' (new or retried)
	Skipped    int      `json:"skipped"`          // already ready/downloading
	Failed     int      `json:"failed"`           // couldn't queue (layout/resolve issue)
	TotalBytes int64    `json:"total_bytes"`      // sum of sizes of the queued leaves
	Errors     []string `json:"errors,omitempty"` // one message per failed leaf
}

// ContainerPreview is the dry-run count behind the "Queue show" confirm
// (glb-gdl.12): how many downloadable leaves would be queued, their total size,
// how many are already mirrored, and how many seasons they span.
type ContainerPreview struct {
	Container   string `json:"container"`
	Source      string `json:"source"`
	ItemID      string `json:"item_id"`
	Kind        string `json:"kind"`
	ToQueue     int    `json:"to_queue"`     // not-yet-mirrored leaves
	AlreadyHave int    `json:"already_have"` // ready/downloading leaves
	Seasons     int    `json:"seasons"`      // distinct season containers spanned
	TotalBytes  int64  `json:"total_bytes"`  // size of the to-queue leaves
}

// BulkEvictResult summarizes a "evict this whole container" operation: every
// mirrored leaf beneath a show/season (or a single leaf) is removed and its
// space freed. Idempotent like its queue counterpart: leaves with no ready local
// copy are counted Skipped and left untouched.
type BulkEvictResult struct {
	Container  string   `json:"container"`        // title of the container evicted
	Evicted    int      `json:"evicted"`          // leaves whose local file was removed
	Skipped    int      `json:"skipped"`          // not mirrored locally (nothing to free)
	Failed     int      `json:"failed"`           // couldn't evict (e.g. path-escape guard)
	FreedBytes int64    `json:"freed_bytes"`      // sum of sizes of the evicted leaves
	Errors     []string `json:"errors,omitempty"` // one message per failed leaf
}

// EvictPreview is the dry-run count behind the "Evict show" confirm: how many
// mirrored leaves would be removed, the space that frees, and how many seasons
// they span.
type EvictPreview struct {
	Container  string `json:"container"`
	Source     string `json:"source"`
	ItemID     string `json:"item_id"`
	Kind       string `json:"kind"`
	ToEvict    int    `json:"to_evict"`    // ready leaves that would be removed
	FreedBytes int64  `json:"freed_bytes"` // space their removal frees
	Seasons    int    `json:"seasons"`     // distinct season containers spanned
}

// QueueContainer enqueues every downloadable leaf beneath a container item,
// descending show → season → episode as needed. Idempotent. Returns
// ErrDownloadUnavailable when no engine is wired, source.ErrUnsupported for a
// non-downloadable source, and ErrChildrenUnavailable when the source can't
// traverse a hierarchy.
func (s *Service) QueueContainer(ctx context.Context, sourceName, itemID string) (BulkQueueResult, error) {
	rt := s.now()
	src, err := rt.downloadableSource(sourceName)
	if err != nil {
		return BulkQueueResult{}, err
	}
	leaves, root, err := s.gatherLeaves(ctx, src, itemID)
	if err != nil {
		return BulkQueueResult{}, err
	}

	// Classify against the pre-existing rows so re-clicking is a clean no-op:
	// already-mirrored / in-flight / already-queued leaves are skipped, and only
	// missing or previously-errored leaves are (re)queued. Engine.Queue resets an
	// 'error' row back to 'queued', so this genuinely retries failures.
	statuses, err := s.statusesForSource(ctx, sourceName)
	if err != nil {
		return BulkQueueResult{}, err
	}

	res := BulkQueueResult{Container: root.Title}
	for _, leaf := range leaves {
		switch statuses[leaf.ID] {
		case "ready", "downloading", "queued":
			res.Skipped++
			continue
		}
		if _, qerr := rt.engine.Queue(ctx, leaf); qerr != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", leafLabel(leaf), qerr))
			continue
		}
		res.Queued++
		res.TotalBytes += leaf.SizeBytes
	}
	return res, nil
}

// PreviewContainer gathers the downloadable leaves under a container without
// queuing anything, for the confirm dialog. Same error contract as
// QueueContainer.
func (s *Service) PreviewContainer(ctx context.Context, sourceName, itemID string) (ContainerPreview, error) {
	src, err := s.now().downloadableSource(sourceName)
	if err != nil {
		return ContainerPreview{}, err
	}
	leaves, root, err := s.gatherLeaves(ctx, src, itemID)
	if err != nil {
		return ContainerPreview{}, err
	}

	statuses, err := s.statusesForSource(ctx, sourceName)
	if err != nil {
		return ContainerPreview{}, err
	}

	prev := ContainerPreview{Container: root.Title, Source: sourceName, ItemID: itemID, Kind: string(root.Kind)}
	seasons := map[string]struct{}{}
	for _, leaf := range leaves {
		switch statuses[leaf.ID] {
		case "ready", "downloading":
			prev.AlreadyHave++
		default:
			prev.ToQueue++
			prev.TotalBytes += leaf.SizeBytes
		}
		if leaf.ParentID != "" {
			seasons[leaf.ParentID] = struct{}{}
		}
	}
	prev.Seasons = len(seasons)
	return prev, nil
}

// EvictContainer evicts every mirrored leaf beneath a container item, descending
// show → season → episode as needed (or evicts a single leaf when itemID is one).
// It deletes each ready local copy and frees the space. Idempotent: leaves with
// no ready local copy are Skipped. Unlike QueueContainer it does NOT require a
// downloadable source — eviction acts on local rows — but it does need the source
// to traverse its hierarchy, so it returns ErrChildrenUnavailable for a source
// that can't (and the usual ErrSourceNotFound / ErrItemNotFound).
func (s *Service) EvictContainer(ctx context.Context, sourceName, itemID string) (BulkEvictResult, error) {
	rt := s.now()
	src, err := rt.source(sourceName)
	if err != nil {
		return BulkEvictResult{}, err
	}
	leaves, root, err := s.gatherLeaves(ctx, src, itemID)
	if err != nil {
		return BulkEvictResult{}, err
	}
	ready, err := s.readyRowsForSource(ctx, sourceName)
	if err != nil {
		return BulkEvictResult{}, err
	}

	res := BulkEvictResult{Container: root.Title}
	for _, leaf := range leaves {
		row, ok := ready[leaf.ID]
		if !ok {
			res.Skipped++
			continue
		}
		ev, eerr := rt.storage.EvictItem(ctx, row.id)
		switch {
		case errors.Is(eerr, storage.ErrItemNotFound):
			// Raced with another evictor / the sweeper; treat as already gone.
			res.Skipped++
		case eerr != nil:
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", leafLabel(leaf), eerr))
		default:
			res.Evicted++
			res.FreedBytes += ev.SizeBytes
		}
	}
	return res, nil
}

// PreviewEvict counts the mirrored leaves under a container without removing
// anything, for the confirm dialog. Same error contract as EvictContainer.
func (s *Service) PreviewEvict(ctx context.Context, sourceName, itemID string) (EvictPreview, error) {
	src, err := s.now().source(sourceName)
	if err != nil {
		return EvictPreview{}, err
	}
	leaves, root, err := s.gatherLeaves(ctx, src, itemID)
	if err != nil {
		return EvictPreview{}, err
	}
	ready, err := s.readyRowsForSource(ctx, sourceName)
	if err != nil {
		return EvictPreview{}, err
	}

	prev := EvictPreview{Container: root.Title, Source: sourceName, ItemID: itemID, Kind: string(root.Kind)}
	seasons := map[string]struct{}{}
	for _, leaf := range leaves {
		row, ok := ready[leaf.ID]
		if !ok {
			continue
		}
		prev.ToEvict++
		prev.FreedBytes += row.size
		if leaf.ParentID != "" {
			seasons[leaf.ParentID] = struct{}{}
		}
	}
	prev.Seasons = len(seasons)
	return prev, nil
}

// readyRow is the slice of an items row a bulk evict needs: where to evict (id)
// and how much it frees (size).
type readyRow struct {
	id   int64
	size int64
}

// readyRowsForSource maps source_key → ready row for one source, so a bulk evict
// resolves each leaf's local id and freed space in a single query (the mirror
// stores no show/season linkage, so the leaf set must come from the source).
func (s *Service) readyRowsForSource(ctx context.Context, sourceName string) (map[string]readyRow, error) {
	rows, err := s.store.QueryContext(ctx,
		`SELECT source_key, id, COALESCE(size_bytes, 0) FROM items WHERE source = ? AND status = 'ready'`, sourceName)
	if err != nil {
		return nil, fmt.Errorf("ready rows for %q: %w", sourceName, err)
	}
	defer rows.Close()
	out := map[string]readyRow{}
	for rows.Next() {
		var key string
		var r readyRow
		if err := rows.Scan(&key, &r.id, &r.size); err != nil {
			return nil, fmt.Errorf("scan ready row: %w", err)
		}
		out[key] = r
	}
	return out, rows.Err()
}

// downloadableSource resolves a source that the engine can actually pull from.
func (r *runtime) downloadableSource(sourceName string) (source.Source, error) {
	if r.engine == nil {
		return nil, ErrDownloadUnavailable
	}
	src, err := r.source(sourceName)
	if err != nil {
		return nil, err
	}
	if _, ok := src.(source.DownloadResolver); !ok {
		return nil, fmt.Errorf("%w: source %q has no download URLs", source.ErrUnsupported, sourceName)
	}
	return src, nil
}

// gatherLeaves returns the container's root metadata plus every downloadable
// leaf beneath it. A leaf is any item carrying a Container (a file); shows and
// seasons are descended. When the root is itself a leaf it's returned as-is.
func (s *Service) gatherLeaves(ctx context.Context, src source.Source, itemID string) ([]source.Item, source.Item, error) {
	root, err := src.GetMetadata(ctx, itemID)
	if err != nil {
		return nil, source.Item{}, err
	}
	if root.Container != "" {
		return []source.Item{root}, root, nil
	}
	lister, ok := src.(source.ChildLister)
	if !ok {
		return nil, root, ErrChildrenUnavailable
	}
	leaves, err := collectLeaves(ctx, lister, root.ID, 0)
	if err != nil {
		return nil, root, err
	}
	return leaves, root, nil
}

// collectLeaves recursively walks a container, returning downloadable leaves.
func collectLeaves(ctx context.Context, lister source.ChildLister, itemID string, depth int) ([]source.Item, error) {
	if depth >= maxChildDepth {
		return nil, nil
	}
	children, err := listAllChildren(ctx, lister, itemID)
	if err != nil {
		return nil, err
	}
	var leaves []source.Item
	for _, c := range children {
		switch {
		case c.Container != "":
			leaves = append(leaves, c)
		case c.Kind == source.ItemShow || c.Kind == source.ItemSeason:
			sub, err := collectLeaves(ctx, lister, c.ID, depth+1)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, sub...)
		}
	}
	return leaves, nil
}

// listAllChildren pages through every child so a large season/show isn't capped
// at a backend's default page size.
func listAllChildren(ctx context.Context, lister source.ChildLister, itemID string) ([]source.Item, error) {
	var all []source.Item
	for offset := 0; offset < childPageSize*1000; offset += childPageSize {
		batch, err := lister.ListChildren(ctx, itemID, source.ListOptions{Limit: childPageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < childPageSize {
			break
		}
	}
	return all, nil
}

// statusesForSource maps source_key → status for every row of one source, so a
// preview can tell which leaves are already mirrored in a single query.
func (s *Service) statusesForSource(ctx context.Context, sourceName string) (map[string]string, error) {
	rows, err := s.store.QueryContext(ctx,
		`SELECT source_key, status FROM items WHERE source = ?`, sourceName)
	if err != nil {
		return nil, fmt.Errorf("statuses for %q: %w", sourceName, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, status string
		if err := rows.Scan(&key, &status); err != nil {
			return nil, fmt.Errorf("scan status: %w", err)
		}
		out[key] = status
	}
	return out, rows.Err()
}

func leafLabel(it source.Item) string {
	if it.ShowTitle != "" {
		return fmt.Sprintf("%s s%02de%02d", it.ShowTitle, it.SeasonNumber, it.EpisodeNumber)
	}
	if it.Title != "" {
		return it.Title
	}
	return it.ID
}
