// Package views holds the Templ components and rendering helpers for the
// plex-mirror web portal (Phase 4). Components are thin: they format values the
// handlers pull from internal/service and never reach into business logic.
//
// The *_templ.go files are generated from the *.templ sources and committed so
// the distroless Docker build (plain `go build`) needs no templ toolchain.
// After editing a .templ, install the matching CLI once
// (`go install github.com/a-h/templ/cmd/templ@v0.3.857`) and run `go generate
// ./...`. The CLI is intentionally not a module/tool dependency so its large
// transitive tree stays out of this service's go.sum.
//
//go:generate templ generate
package views

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/maxdubrinsky/plex-mirror/internal/service"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

// kindLabel renders a source item/library kind as a capitalized word.
func kindLabel(k string) string {
	if k == "" {
		return ""
	}
	return strings.ToUpper(k[:1]) + k[1:]
}

// yearLabel renders a release year, or "—" when unknown (0), so the table reads
// cleanly.
func yearLabel(year int) string {
	if year <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d", year)
}

// itemMeta builds the muted secondary line on an item card: kind, year, size,
// container — only the parts that are populated.
func itemMeta(it source.Item) string {
	parts := make([]string, 0, 4)
	if l := kindLabel(string(it.Kind)); l != "" {
		parts = append(parts, l)
	}
	if it.Year > 0 {
		parts = append(parts, fmt.Sprintf("%d", it.Year))
	}
	if it.SizeBytes > 0 {
		parts = append(parts, HumanBytes(it.SizeBytes))
	}
	if it.Container != "" {
		parts = append(parts, strings.ToUpper(it.Container))
	}
	return strings.Join(parts, " · ")
}

// canQueue reports whether to offer a Queue button: source downloadable, item
// is file-backed (has a container), and it isn't already in a non-requeueable
// mirror state.
func canQueue(downloadable bool, status, container string) bool {
	return downloadable && container != "" && itemQueueable(downloadable, status)
}

// browseHref builds a /browse link with optional source/library query params,
// URL-encoded. Returned as templ.SafeURL so templ renders it into href without
// re-escaping.
func browseHref(source, library string) templ.SafeURL {
	q := url.Values{}
	if source != "" {
		q.Set("source", source)
	}
	if library != "" {
		q.Set("library", library)
	}
	u := "/browse"
	if e := q.Encode(); e != "" {
		u += "?" + e
	}
	return templ.SafeURL(u)
}

// itemHref builds the /item detail link for a (source, id) pair.
func itemHref(source, id string) templ.SafeURL {
	q := url.Values{"source": {source}, "id": {id}}
	return templ.SafeURL("/item?" + q.Encode())
}

// queueURL / cancelURL / evictURL are the hx-post targets for the action
// buttons. They feed hx-post (not href), so a plain encoded string is fine;
// handlers read params via r.FormValue (query + body).
func queueURL(source, id string) string {
	q := url.Values{"source": {source}, "item": {id}}
	return "/queue?" + q.Encode()
}

func cancelURL(id int64) string {
	return fmt.Sprintf("/cancel?id=%d", id)
}

// queueContainerURL is the hx-post target that queues every episode under a
// season/show; queueContainerConfirmURL is the hx-get that returns the
// count/size confirm dialog first (glb-gdl.11/.12).
func queueContainerURL(source, id string) string {
	q := url.Values{"source": {source}, "item": {id}}
	return "/queue/container?" + q.Encode()
}

func queueContainerConfirmURL(source, id string) string {
	q := url.Values{"source": {source}, "item": {id}}
	return "/queue/container/confirm?" + q.Encode()
}

// seasonQueueConfirm is the browser confirm() text for the season bulk button.
func seasonQueueConfirm(count int, bytes int64) string {
	if count <= 0 {
		return "Re-check this season for episodes to queue?"
	}
	return fmt.Sprintf("Queue %d episode(s) (%s) to the local mirror?", count, HumanBytes(bytes))
}

// childrenURL re-fetches the item's children panel as a fragment (used to close
// the show confirm dialog and to refresh per-episode badges).
func childrenURL(source, id string) string {
	q := url.Values{"source": {source}, "id": {id}}
	return "/item/children?" + q.Encode()
}

// bulkErrorTitle joins the per-leaf failure messages into a tooltip for the
// "N failed" badge.
func bulkErrorTitle(r *service.BulkQueueResult) string {
	if r == nil || len(r.Errors) == 0 {
		return "some items could not be queued"
	}
	return strings.Join(r.Errors, "\n")
}

func evictURL(id int64) string {
	return fmt.Sprintf("/evict?id=%d", id)
}

// browseItemsURL builds the #results fragment URL for a pager step, carrying
// the active filter + view so paging doesn't drop the search or the render mode.
func browseItemsURL(vm BrowseVM, offset int) string {
	q := url.Values{"source": {vm.Source}, "library": {vm.Library}}
	if vm.Filter != "" {
		q.Set("filter", vm.Filter)
	}
	if vm.View != "" {
		q.Set("view", vm.View)
	}
	q.Set("offset", fmt.Sprintf("%d", offset))
	return "/browse/items?" + q.Encode()
}

// browseSearchURL is the search box's hx-get base. It omits filter on purpose —
// the input supplies its own value as the `filter` param — and resets offset.
func browseSearchURL(vm BrowseVM) string {
	q := url.Values{"source": {vm.Source}, "library": {vm.Library}, "offset": {"0"}}
	if vm.View != "" {
		q.Set("view", vm.View)
	}
	return "/browse/items?" + q.Encode()
}

// browseViewURL is the #results fragment URL for switching the results render
// mode (card/table), preserving the current source/library/filter/offset.
func browseViewURL(vm BrowseVM, view string) string {
	q := url.Values{"source": {vm.Source}, "library": {vm.Library}, "view": {view}}
	if vm.Filter != "" {
		q.Set("filter", vm.Filter)
	}
	q.Set("offset", fmt.Sprintf("%d", vm.Offset))
	return "/browse/items?" + q.Encode()
}

// viewToggleClass marks the active view-mode button in the card/table switch.
func viewToggleClass(active bool) string {
	if active {
		return "vtoggle active"
	}
	return "vtoggle"
}

// libKindIcon returns a small mono glyph for a library kind, decorating the
// sidebar rail. Purely decorative — the library title is the real label.
func libKindIcon(kind string) string {
	switch source.LibraryKind(kind) {
	case source.LibraryMovies:
		return "►"
	case source.LibraryShows:
		return "▣"
	case source.LibraryMusic:
		return "♪"
	default:
		return "◆"
	}
}

// sidebarLinkClass styles a sidebar entry (library or utility link), marking the
// active one.
func sidebarLinkClass(active bool) string {
	if active {
		return "nav-item active"
	}
	return "nav-item"
}

// iconClass styles a topbar icon button (e.g. the settings gear), marking the
// active section.
func iconClass(active bool) string {
	if active {
		return "iconbtn active"
	}
	return "iconbtn"
}

// secretPlaceholder is the password-field hint on the settings page: when a
// token is already stored, blank means "keep it"; otherwise prompt to enter one.
func secretPlaceholder(isSet bool) string {
	if isSet {
		return "•••••• (leave blank to keep current)"
	}
	return "enter token"
}

// clearFieldName is the checkbox name that wipes a stored secret, paired with
// its password field (e.g. "plex_token" → "clear_plex_token").
func clearFieldName(name string) string {
	return "clear_" + name
}

// navClass marks the active top-nav link.
func navClass(active, key string) string {
	if active == key {
		return "active"
	}
	return ""
}

// HumanBytes formats a byte count with 1024-based units to match how config
// parses storage caps (so a "10G" cap and its usage gauge speak the same
// units). Returns "—" for non-positive values so empty cells read cleanly.
func HumanBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// pctStyle is an inline width style for a progress bar fill, e.g. "width:42%".
func pctStyle(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf("width:%d%%", pct)
}

// Pct turns a 0..1 fraction into a clamped 0..100 integer for bar widths.
func Pct(fraction float64) int {
	p := int(fraction*100 + 0.5)
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// StatusClass maps a mirror status to a CSS class so the stylesheet owns the
// colors rather than scattering inline styles through the markup.
func StatusClass(status string) string {
	switch status {
	case "ready":
		return "badge badge-ready"
	case "downloading":
		return "badge badge-active"
	case "queued":
		return "badge badge-queued"
	case "error":
		return "badge badge-error"
	case "evicted":
		return "badge badge-muted"
	default:
		return "badge"
	}
}

// RelTime renders a unix timestamp as a short relative string ("3m ago"). A
// zero/absent timestamp yields "—".
func RelTime(unix int64) string {
	if unix <= 0 {
		return "—"
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// --- source health diagnostics ---------------------------------------------

// healthStatusClass maps a source health status to a badge CSS class, reusing the
// mirror-status palette so the colors stay consistent.
func healthStatusClass(status string) string {
	switch status {
	case "ok":
		return "badge badge-ready"
	case "down":
		return "badge badge-error"
	case "auth_error", "parked":
		return "badge badge-queued"
	default:
		return "badge badge-muted"
	}
}

// healthAgo renders how long ago t happened ("3m ago"), or "—" when unset.
func healthAgo(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// healthUntil renders the wait until t ("in 12s"), "now" when due/past, or "—"
// when unset — used for the auto-retry countdown.
func healthUntil(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return fmt.Sprintf("in %ds", int(d.Seconds())+1)
	}
	return fmt.Sprintf("in %dm", int(d.Minutes())+1)
}

// reachableLabel renders a candidate's tri-state probe result.
func reachableLabel(reachable *bool) string {
	switch {
	case reachable == nil:
		return "not probed"
	case *reachable:
		return "reachable"
	default:
		return "unreachable"
	}
}

// reachableClass styles the candidate reachability cell.
func reachableClass(reachable *bool) string {
	switch {
	case reachable == nil:
		return "badge badge-muted"
	case *reachable:
		return "badge badge-ready"
	default:
		return "badge badge-error"
	}
}

// healthHeadline is the short banner sentence for an unhealthy source.
func healthHeadline(h *service.SourceHealthView) string {
	if h == nil {
		return ""
	}
	switch h.Status {
	case "auth_error":
		return fmt.Sprintf("%s authentication failed", h.Name)
	case "parked":
		return fmt.Sprintf("%s is misconfigured", h.Name)
	default:
		return fmt.Sprintf("%s is offline", h.Name)
	}
}

// reconnectURL is the hx-post target for the per-source "Reconnect now" button.
func reconnectURL() string { return "/settings/reconnect" }

// candidateType labels a Plex candidate connection by how it reaches the server.
func candidateType(c service.CandidateView) string {
	switch {
	case c.Relay:
		return "relay"
	case c.Local:
		return "local"
	default:
		return "remote"
	}
}

// itemQueueable reports whether the browse grid should offer a Queue button for
// an item: the source must be downloadable and the item not already in the
// mirror in a state where re-queuing is a no-op (queued/downloading/ready).
func itemQueueable(downloadable bool, status string) bool {
	if !downloadable {
		return false
	}
	switch status {
	case "queued", "downloading", "ready":
		return false
	default:
		return true
	}
}

// gaugeClass adds the "over" modifier when usage has crossed a configured cap,
// so the storage gauge shifts to a warning gradient.
func gaugeClass(vm StorageVM) templ.CSSClasses {
	return templ.Classes("gauge", templ.KV("over", overCap(vm.Stats)))
}

// capLabel renders a configured cap, or "Off" when unset (0).
func capLabel(n int64) string {
	if n <= 0 {
		return "Off"
	}
	return HumanBytes(n)
}

// overCap reports whether usage has crossed a configured cap (soft preferred,
// else hard), used to flip the gauge into its warning gradient.
func overCap(s service.StorageStats) bool {
	if s.SoftCapBytes > 0 {
		return s.UsedBytes > s.SoftCapBytes
	}
	if s.HardCapBytes > 0 {
		return s.UsedBytes > s.HardCapBytes
	}
	return false
}

// gaugeMax is the denominator for the usage gauge: the hard cap when set,
// otherwise used+free (the physical ceiling). Never zero.
func gaugeMax(s service.StorageStats) int64 {
	if s.HardCapBytes > 0 {
		return s.HardCapBytes
	}
	if m := s.UsedBytes + s.FreeBytes; m > 0 {
		return m
	}
	return 1
}

// usedPct is the gauge fill percentage for the storage view.
func usedPct(s service.StorageStats) int {
	return Pct(float64(s.UsedBytes) / float64(gaugeMax(s)))
}
