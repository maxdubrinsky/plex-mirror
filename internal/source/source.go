// Package source defines the cross-backend browse interface used by both the
// Plex and Jellyfin adapters. Download resolution is split out so only the
// adapters that actually hand back streamable bytes need to implement it.
package source

import (
	"context"
	"errors"
	"net/http"
)

// LibraryKind classifies a library at a glance so the UI can pick icons /
// columns without round-tripping back through the adapter.
type LibraryKind string

const (
	LibraryMovies LibraryKind = "movies"
	LibraryShows  LibraryKind = "shows"
	LibraryMusic  LibraryKind = "music"
	LibraryOther  LibraryKind = "other"
)

// ItemKind is the catalog-level item type, not a per-file kind. A show entry
// is one Item; its episodes would be separate Items addressable by ID.
type ItemKind string

const (
	ItemMovie   ItemKind = "movie"
	ItemShow    ItemKind = "show"
	ItemSeason  ItemKind = "season"
	ItemEpisode ItemKind = "episode"
	ItemTrack   ItemKind = "track"
	ItemOther   ItemKind = "other"
)

// Library is the top-level browseable container on a source.
type Library struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Kind  LibraryKind `json:"kind"`
}

// Item is the cross-source media descriptor. Fields are intentionally lossy —
// each adapter populates what it has; consumers should not assume non-zero.
type Item struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Kind      ItemKind `json:"kind"`
	Year      int      `json:"year,omitempty"`
	Container string   `json:"container,omitempty"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	// Played is tri-state: nil = source does not expose watched state.
	// Used by storage manager's LRU policy once Jellyfin is wired in.
	Played *bool `json:"played,omitempty"`

	// Episode-only fields. Populated when Kind == ItemEpisode so the
	// download layout can build shows/<show>/Season NN/ paths. Zero-valued
	// for non-episode items.
	ShowTitle     string `json:"show_title,omitempty"`
	SeasonNumber  int    `json:"season_number,omitempty"`
	EpisodeNumber int    `json:"episode_number,omitempty"`

	// Hierarchy links for breadcrumb navigation up the container tree. Adapters
	// populate whatever the backend exposes; all zero-valued for roots/leaves
	// where they don't apply (a movie has neither, a show has none, a season has
	// only Parent = its show, an episode has Parent = season + Grandparent =
	// show). The *Title fields label the crumb; ShowTitle doubles as the
	// grandparent (show) label for episodes.
	ParentID      string `json:"parent_id,omitempty"`      // episode→season, season→show
	ParentTitle   string `json:"parent_title,omitempty"`   // label for ParentID
	GrandparentID string `json:"grandparent_id,omitempty"` // episode→show; empty otherwise
	// LibraryID/LibraryTitle identify the owning library/section when the adapter
	// can supply it cheaply (Plex does; Jellyfin leaves them empty), so the
	// breadcrumb root can deep-link to the library rather than the source top.
	LibraryID    string `json:"library_id,omitempty"`
	LibraryTitle string `json:"library_title,omitempty"`
}

// ListOptions controls pagination / shaping on ListItems. Adapters that don't
// support an option should silently ignore it rather than erroring.
type ListOptions struct {
	Offset int
	Limit  int    // 0 = adapter default
	Query  string // title search; "" = no filter. Pushed to the backend so it
	// spans the whole library, not just the current page. Adapters that can't
	// search server-side should ignore it (the service applies a client-side
	// title filter as a backstop).
}

// Source is the browse surface. Both PlexSource and JellyfinSource implement
// it. ResolveDownloadURL is intentionally NOT part of this interface — only
// Plex hands back downloadable bytes; Jellyfin is local-mirror inventory.
type Source interface {
	// Name returns a stable, human-readable identifier, e.g. "plex" or "jellyfin".
	Name() string
	ListLibraries(ctx context.Context) ([]Library, error)
	ListItems(ctx context.Context, libraryID string, opts ListOptions) ([]Item, error)
	GetMetadata(ctx context.Context, itemID string) (Item, error)
}

// DownloadTarget is what the download engine needs to fetch the bytes: a URL
// to GET and any headers (auth, etc) it should attach.
type DownloadTarget struct {
	URL     string
	Headers http.Header
}

// DownloadResolver is the optional capability for sources that can yield
// streamable bytes. PlexSource implements it; JellyfinSource does not.
type DownloadResolver interface {
	ResolveDownloadURL(ctx context.Context, itemID string) (*DownloadTarget, error)
}

// ChildLister is the optional capability for sources with a container hierarchy:
// it returns the direct children of an item (show → seasons, season → episodes,
// artist → albums, album → tracks). Leaf items (movies, episodes) have none.
// Both Plex and Jellyfin implement it; the service degrades gracefully with
// ErrUnsupported for any source that doesn't.
type ChildLister interface {
	ListChildren(ctx context.Context, itemID string, opts ListOptions) ([]Item, error)
}

// Typed errors. Adapters wrap these with fmt.Errorf("...: %w", Err...) so
// callers can errors.Is against the sentinel without losing context.
var (
	ErrAuth        = errors.New("source: authentication failed")
	ErrNotFound    = errors.New("source: not found")
	ErrNetwork     = errors.New("source: network error")
	ErrUnsupported = errors.New("source: operation not supported")
)
