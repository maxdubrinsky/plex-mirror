package service

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/maxdubrinsky/plex-mirror/internal/download"
)

// ErrCannotCancel means the item is in a state where Cancel doesn't apply: an
// in-flight 'downloading' row can't be safely interrupted (the engine holds
// it), and 'ready'/'evicted' rows are Evict's job, not Cancel's.
var ErrCannotCancel = errors.New("service: item cannot be cancelled in its current state")

// Cancel removes a not-yet-downloaded item from the queue. Allowed only for
// 'queued' and 'error' rows; it deletes the row and best-effort removes any
// leftover partial file (an errored item may have one). Returns ErrItemNotFound
// for an unknown id and ErrCannotCancel for a non-cancellable state.
func (s *Service) Cancel(ctx context.Context, id int64) error {
	it, err := s.itemByID(ctx, id)
	if err != nil {
		return err
	}
	switch it.Status {
	case "queued", "error":
		// cancellable
	case "downloading":
		return fmt.Errorf("%w: download is in flight", ErrCannotCancel)
	default:
		return fmt.Errorf("%w: status=%s (use evict for ready items)", ErrCannotCancel, it.Status)
	}

	if _, err := s.store.ExecContext(ctx, `DELETE FROM items WHERE id = ?`, id); err != nil {
		return fmt.Errorf("cancel: delete row %d: %w", id, err)
	}

	// Reuse the engine's exact partial-path scheme (sha256 of source:key) so we
	// never duplicate the hashing; this leaves no orphaned .partials file behind.
	layout := download.Layout{MediaRoot: s.now().cfg.MediaRoot}
	partial := layout.Partial(it.Source, it.SourceKey)
	if rmErr := os.Remove(partial); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		// Non-fatal: the row is already gone; a stray partial is harmless and
		// will be reused/cleaned if the item is re-queued.
		return nil
	}
	return nil
}
