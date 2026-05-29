package service

import (
	"context"
	"fmt"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
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
