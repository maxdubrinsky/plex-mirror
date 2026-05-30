package views

import (
	"github.com/a-h/templ"

	"github.com/maxdubrinsky/plex-mirror/internal/service"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

// PageMeta is the chrome context every full page needs: tab title, which nav
// item is active, whether app-level auth is on (controls the Sign out link), and
// the sidebar navigation tree (sources → libraries) plus which library is
// currently selected so the rail can highlight it.
type PageMeta struct {
	Title       string
	Active      string // "browse" | "storage" | "settings"
	AuthEnabled bool

	// Nav is the sidebar tree: one entry per configured source, each carrying its
	// libraries (or a per-source error). ActiveSource/ActiveLibrary mark the
	// selected leaf so @sidebar can highlight it.
	Nav           []NavSource
	ActiveSource  string
	ActiveLibrary string
}

// NavSource is one source group in the sidebar: its name, whether it's
// downloadable, and either its libraries or a fetch error (so one unreachable
// source degrades to an inline note instead of failing the whole page).
type NavSource struct {
	Name         string
	Downloadable bool
	Libraries    []source.Library
	Err          string
}

// libActive reports whether (src, lib) is the currently selected library, so the
// sidebar can mark the active rail entry.
func (m PageMeta) libActive(src, lib string) bool {
	return m.ActiveSource == src && m.ActiveLibrary == lib
}

// BrowseVM drives the browse page and its #results fragment. Selected source /
// library are empty until the user drills in. Statuses maps a source item id to
// its current mirror status so cards can badge "ready"/"queued"/etc.
type BrowseVM struct {
	Sources      []service.SourceInfo
	Source       string
	Downloadable bool
	Library      string
	LibraryTitle string
	Items        []source.Item
	Statuses     map[string]string
	Filter       string
	Offset       int
	Limit        int
	HasMore      bool   // a full page came back, so a Next page may exist
	View         string // "card" | "table" — results rendering mode
	Err          string
	// Health is the active source's health when it isn't ok, so the page can show a
	// "source offline — auto-reconnecting" banner with a link to the diagnostics.
	// nil when the source is healthy (or none is selected).
	Health *service.SourceHealthView
}

func (vm BrowseVM) status(itemID string) string { return vm.Statuses[itemID] }

func (vm BrowseVM) prevOffset() int {
	if vm.Offset-vm.Limit < 0 {
		return 0
	}
	return vm.Offset - vm.Limit
}

func (vm BrowseVM) nextOffset() int { return vm.Offset + vm.Limit }

func (vm BrowseVM) pageNum() int {
	if vm.Limit <= 0 {
		return 1
	}
	return vm.Offset/vm.Limit + 1
}

// ItemDetailVM drives the item detail page. Mirror is nil when the item has no
// local row yet. Children/ChildStatuses are populated for container items
// (show/season) so the page can drill down to the downloadable leaves.
type ItemDetailVM struct {
	Source        string
	Item          source.Item
	Mirror        *service.MirrorItem
	Downloadable  bool
	Children      []source.Item
	ChildStatuses map[string]string
	// Bulk is set after a "Queue season"/"Queue show" action so the children
	// panel can render a result banner (glb-gdl.11/.12). nil on first paint.
	Bulk *service.BulkQueueResult
	Err  string
}

// bulkQueueable reports whether to offer a bulk "Queue season/show" control:
// the source must be downloadable and this item a show/season container.
func (vm ItemDetailVM) bulkQueueable() bool {
	return vm.Downloadable &&
		(vm.Item.Kind == source.ItemShow || vm.Item.Kind == source.ItemSeason)
}

// queueableChildren counts the children that carry a file and aren't already
// mirrored/in-flight, with their total size — used for the season confirm text
// (the season page already has its episodes loaded, so no round-trip needed).
func (vm ItemDetailVM) queueableChildren() (count int, bytes int64) {
	for _, c := range vm.Children {
		if c.Container == "" {
			continue
		}
		switch vm.childStatus(c.ID) {
		case "ready", "downloading", "queued":
			continue
		}
		count++
		bytes += c.SizeBytes
	}
	return count, bytes
}

func (vm ItemDetailVM) status() string {
	if vm.Mirror != nil {
		return vm.Mirror.Status
	}
	return ""
}

// isContainer reports whether this item holds children rather than a file, so
// the page shows a drill-down grid instead of the queue/cancel mirror panel.
func (vm ItemDetailVM) isContainer() bool {
	return vm.Item.Kind == source.ItemShow || vm.Item.Kind == source.ItemSeason
}

func (vm ItemDetailVM) childStatus(itemID string) string { return vm.ChildStatuses[itemID] }

// crumb is one hop in the item-detail breadcrumb trail.
type crumb struct {
	Label string
	Href  templ.SafeURL
}

// breadcrumbTrail returns the ancestor hops for the item page, root-first and
// excluding the current item (the page <h1> renders that). The root deep-links
// to the owning library when the adapter supplied one, else the source's browse
// top. Episodes get Show › Season; seasons get Show; movies/leaves get just the
// root — so the show→season→episode drill-down is no longer a one-way trip
// (glb-gdl.10).
func (vm ItemDetailVM) breadcrumbTrail() []crumb {
	trail := []crumb{vm.rootCrumb()}
	switch vm.Item.Kind {
	case source.ItemEpisode:
		if vm.Item.GrandparentID != "" {
			trail = append(trail, crumb{
				Label: crumbLabel(vm.Item.ShowTitle, "Show"),
				Href:  itemHref(vm.Source, vm.Item.GrandparentID),
			})
		}
		if vm.Item.ParentID != "" {
			trail = append(trail, crumb{
				Label: crumbLabel(vm.Item.ParentTitle, "Season"),
				Href:  itemHref(vm.Source, vm.Item.ParentID),
			})
		}
	case source.ItemSeason:
		if vm.Item.ParentID != "" {
			trail = append(trail, crumb{
				Label: crumbLabel(vm.Item.ParentTitle, "Show"),
				Href:  itemHref(vm.Source, vm.Item.ParentID),
			})
		}
	}
	return trail
}

// rootCrumb is the leftmost breadcrumb: the owning library when known, else the
// source's browse top (the old "Back to browse" target).
func (vm ItemDetailVM) rootCrumb() crumb {
	if vm.Item.LibraryID != "" {
		return crumb{
			Label: crumbLabel(vm.Item.LibraryTitle, "Library"),
			Href:  browseHref(vm.Source, vm.Item.LibraryID),
		}
	}
	return crumb{Label: "Browse", Href: browseHref(vm.Source, "")}
}

func crumbLabel(title, fallback string) string {
	if title == "" {
		return fallback
	}
	return title
}

// childrenHeading labels the drill-down grid based on what this container holds.
func (vm ItemDetailVM) childrenHeading() string {
	switch vm.Item.Kind {
	case source.ItemShow:
		return "Seasons"
	case source.ItemSeason:
		return "Episodes"
	default:
		return "Contents"
	}
}

// StorageVM drives the storage page and its #storage-panel fragment.
type StorageVM struct {
	Stats service.StorageStats
	Items []service.MirrorItem // ready inventory
	Err   string
}

// SettingsVM drives the settings page and its #settings-form fragment
// (glb-gdl.13). Config holds the current effective, secret-free configuration.
// The three banner states are mutually exclusive: Err (validation/persist
// failure — nothing changed), SavedNotLive (saved + persisted but the live
// reload failed; Warn carries the detail), or Saved (saved and applied live).
type SettingsVM struct {
	Config       service.ConfigView
	Saved        bool
	SavedNotLive bool
	Warn         string
	Err          string
	// Health is each configured source's connectivity diagnostics, rendered in the
	// source-health panel below the form (and refreshed by the reconnect button).
	Health []service.SourceHealthView
}
