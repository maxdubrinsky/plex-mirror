package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/maxdubrinsky/plex-mirror/internal/server/views"
	"github.com/maxdubrinsky/plex-mirror/internal/service"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

const (
	authCookieName   = "pm_auth"
	viewCookieName   = "pm_view"
	defaultPageLimit = 50
	navCacheTTL      = 60 * time.Second
)

// portalMux wires the browser-portal routes. It is mounted behind portalAuth on
// the catch-all "/" so it handles everything except /healthz, /mcp, and the
// public /login + /static endpoints.
func (s *Server) portalMux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /", s.handleIndex)
	m.HandleFunc("GET /browse", s.handleBrowse)
	m.HandleFunc("GET /browse/items", s.handleBrowseItems)
	m.HandleFunc("GET /item", s.handleItem)
	m.HandleFunc("GET /item/children", s.handleItemChildren)
	m.HandleFunc("POST /queue", s.handleQueue)
	m.HandleFunc("POST /queue/container", s.handleQueueContainer)
	m.HandleFunc("GET /queue/container/confirm", s.handleQueueContainerConfirm)
	m.HandleFunc("GET /queue", s.handleQueuePage)
	m.HandleFunc("GET /queue/rows", s.handleQueueRows)
	m.HandleFunc("GET /queue/drawer", s.handleQueueDrawer)
	m.HandleFunc("GET /queue/count", s.handleQueueCount)
	m.HandleFunc("POST /cancel", s.handleCancel)
	m.HandleFunc("POST /evict", s.handleEvict)
	m.HandleFunc("GET /storage", s.handleStoragePage)
	m.HandleFunc("GET /settings", s.handleSettingsPage)
	m.HandleFunc("POST /settings", s.handleSettingsSave)
	return m
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/browse", http.StatusFound)
}

// --- Browse ---------------------------------------------------------------

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	vm := views.BrowseVM{
		Sources: s.svc.ListSources(),
		Source:  q.Get("source"),
		Library: q.Get("library"),
		Filter:  q.Get("filter"),
		Offset:  atoiDefault(q.Get("offset"), 0),
		Limit:   defaultPageLimit,
		View:    s.resolveView(w, r),
	}
	if vm.Source != "" {
		vm.Downloadable = downloadableFor(vm.Sources, vm.Source)
		// Reuse the (cached) sidebar fetch for the active source's libraries so we
		// don't double round-trip — shell() reads the same cache below.
		libs, errMsg := s.navLibraries(ctx, vm.Source)
		if errMsg != "" {
			vm.Err = "This source is currently unavailable."
		} else if vm.Library != "" {
			vm.LibraryTitle = libraryTitle(libs, vm.Library)
			s.loadItems(ctx, &vm)
		}
	}
	title := "Browse"
	if vm.LibraryTitle != "" {
		title = vm.LibraryTitle
	}
	render(w, r, views.BrowsePage(s.shell(ctx, title, "browse", vm.Source, vm.Library), vm))
}

// handleBrowseItems serves the #results fragment for search + pagination + the
// view toggle. It pushes the canonical full-page URL so back/forward and reload
// reconstruct this exact state via handleBrowse.
func (s *Server) handleBrowseItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	vm := views.BrowseVM{
		Source:       q.Get("source"),
		Library:      q.Get("library"),
		Filter:       q.Get("filter"),
		Offset:       atoiDefault(q.Get("offset"), 0),
		Limit:        defaultPageLimit,
		View:         s.resolveView(w, r),
		Downloadable: downloadableFor(s.svc.ListSources(), q.Get("source")),
	}
	s.loadItems(ctx, &vm)
	w.Header().Set("HX-Push-Url", canonicalBrowseURL(vm))
	render(w, r, views.BrowseItemsFragment(vm))
}

// canonicalBrowseURL is the full-page /browse URL equivalent to a #results
// fragment state, pushed via HX-Push-Url so the address bar stays shareable and
// back/forward works. Defaults (card view, offset 0) are omitted for clean URLs.
func canonicalBrowseURL(vm views.BrowseVM) string {
	q := url.Values{"source": {vm.Source}, "library": {vm.Library}}
	if vm.Filter != "" {
		q.Set("filter", vm.Filter)
	}
	if vm.View == "table" {
		q.Set("view", vm.View)
	}
	if vm.Offset > 0 {
		q.Set("offset", strconv.Itoa(vm.Offset))
	}
	return "/browse?" + q.Encode()
}

// loadItems fills the items page + per-item mirror statuses on a BrowseVM.
func (s *Server) loadItems(ctx context.Context, vm *views.BrowseVM) {
	items, err := s.svc.ListItems(ctx, vm.Source, vm.Library, vm.Filter, vm.Limit, vm.Offset)
	if err != nil {
		vm.Err = err.Error()
		return
	}
	vm.Items = items
	// A full page implies there may be a next one. Title-filtered pages can
	// under-count (filter runs after the adapter's page), so Next may show one
	// empty page — acceptable for a homelab browser.
	vm.HasMore = len(items) == vm.Limit
	vm.Statuses = s.statusMap(ctx, vm.Source)
}

// statusMap maps source item id -> mirror status for one source, so browse
// cards can badge already-mirrored / in-flight items.
func (s *Server) statusMap(ctx context.Context, srcName string) map[string]string {
	m := map[string]string{}
	if inflight, err := s.svc.DownloadStatus(ctx, nil); err == nil {
		for _, it := range inflight {
			if it.Source == srcName {
				m[it.SourceKey] = it.Status
			}
		}
	}
	if ready, err := s.svc.ListMirrored(ctx, ""); err == nil {
		for _, it := range ready {
			if it.Source == srcName {
				m[it.SourceKey] = it.Status
			}
		}
	}
	return m
}

// --- Item detail ----------------------------------------------------------

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srcName := r.URL.Query().Get("source")
	id := r.URL.Query().Get("id")
	vm, err := s.buildItemVM(ctx, srcName, id)
	if err != nil {
		vm = views.ItemDetailVM{
			Source: srcName,
			Item:   source.Item{ID: id, Title: "(unavailable)"},
			Err:    err.Error(),
		}
	}
	title := vm.Item.Title
	if title == "" {
		title = "Item"
	}
	render(w, r, views.ItemDetailPage(s.shell(ctx, title, "browse", srcName, vm.Item.LibraryID), vm))
}

func (s *Server) buildItemVM(ctx context.Context, srcName, id string) (views.ItemDetailVM, error) {
	item, err := s.svc.GetItem(ctx, srcName, id)
	if err != nil {
		return views.ItemDetailVM{}, err
	}
	vm := views.ItemDetailVM{
		Source:       srcName,
		Item:         item,
		Downloadable: downloadableFor(s.svc.ListSources(), srcName),
		Mirror:       s.findMirror(ctx, srcName, id),
	}
	// A container (show/season) has no file of its own; show its children so the
	// user can drill down to the episodes that are actually downloadable.
	if item.Kind == source.ItemShow || item.Kind == source.ItemSeason {
		children, cerr := s.svc.ListChildren(ctx, srcName, id, 0, 0)
		switch {
		case cerr == nil:
			vm.Children = children
			vm.ChildStatuses = s.statusMap(ctx, srcName)
		case errors.Is(cerr, service.ErrChildrenUnavailable):
			// Source can't traverse a hierarchy; fall back to the mirror panel.
		default:
			vm.Err = cerr.Error()
		}
	}
	return vm, nil
}

// findMirror returns the local row for a (source, key) if one exists in any
// active or ready state, else nil.
func (s *Server) findMirror(ctx context.Context, srcName, key string) *service.MirrorItem {
	if inflight, err := s.svc.DownloadStatus(ctx, nil); err == nil {
		for i := range inflight {
			if inflight[i].Source == srcName && inflight[i].SourceKey == key {
				return &inflight[i]
			}
		}
	}
	if ready, err := s.svc.ListMirrored(ctx, ""); err == nil {
		for i := range ready {
			if ready[i].Source == srcName && ready[i].SourceKey == key {
				return &ready[i]
			}
		}
	}
	return nil
}

// handleItemChildren re-renders just the children panel for a container item.
// Used to refresh per-episode badges and to dismiss the "Queue show" confirm
// dialog without a full page reload.
func (s *Server) handleItemChildren(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vm, err := s.buildItemVM(ctx, r.URL.Query().Get("source"), r.URL.Query().Get("id"))
	if err != nil {
		render(w, r, views.InlineError(err.Error()))
		return
	}
	render(w, r, views.ChildrenPanel(vm))
}

// --- Mutations: queue / cancel / evict ------------------------------------

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srcName := r.FormValue("source")
	itemID := r.FormValue("item")
	mi, err := s.svc.QueueDownload(ctx, srcName, itemID)
	if err != nil {
		render(w, r, views.InlineError(err.Error()))
		return
	}
	if r.Header.Get("HX-Target") == "mirror-panel" {
		s.renderMirrorPanel(w, r, srcName, itemID)
		return
	}
	dl := downloadableFor(s.svc.ListSources(), srcName)
	render(w, r, views.ActionCell(srcName, mi.SourceKey, mi.Container, dl, mi.Status))
}

// handleQueueContainer queues every downloadable episode under a season/show
// and re-renders the children panel with a result banner (glb-gdl.11/.12).
func (s *Server) handleQueueContainer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srcName := r.FormValue("source")
	itemID := r.FormValue("item")
	res, err := s.svc.QueueContainer(ctx, srcName, itemID)
	if err != nil {
		render(w, r, views.InlineError(err.Error()))
		return
	}
	vm, verr := s.buildItemVM(ctx, srcName, itemID)
	if verr != nil {
		render(w, r, views.InlineError(verr.Error()))
		return
	}
	vm.Bulk = &res
	render(w, r, views.ChildrenPanel(vm))
}

// handleQueueContainerConfirm returns the show-level confirm dialog (episode
// count + total size) before a bulk queue actually runs (glb-gdl.12).
func (s *Server) handleQueueContainerConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	prev, err := s.svc.PreviewContainer(ctx, q.Get("source"), q.Get("item"))
	if err != nil {
		render(w, r, views.InlineError(err.Error()))
		return
	}
	render(w, r, views.QueueConfirm(prev))
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	// Capture source/key before the row is deleted so the detail panel can be
	// rebuilt from the source afterward.
	srcName, key := s.sourceKeyFor(ctx, id)
	if err := s.svc.Cancel(ctx, id); err != nil {
		render(w, r, views.InlineError(err.Error()))
		return
	}
	if r.Header.Get("HX-Target") == "mirror-panel" {
		s.renderMirrorPanel(w, r, srcName, key)
		return
	}
	// Queue page: an empty 200 body lets the htmx outerHTML swap drop the row.
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleEvict(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	srcName, key := s.sourceKeyFor(ctx, id)
	if _, err := s.svc.Evict(ctx, id); err != nil {
		render(w, r, views.InlineError(err.Error()))
		return
	}
	switch r.Header.Get("HX-Target") {
	case "storage-panel":
		render(w, r, views.StoragePanel(s.storageVM(ctx)))
	case "mirror-panel":
		s.renderMirrorPanel(w, r, srcName, key)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// renderMirrorPanel rebuilds and renders the item detail mirror panel, falling
// back to an inline error if the item can no longer be fetched.
func (s *Server) renderMirrorPanel(w http.ResponseWriter, r *http.Request, srcName, key string) {
	vm, err := s.buildItemVM(r.Context(), srcName, key)
	if err != nil {
		render(w, r, views.InlineError(err.Error()))
		return
	}
	render(w, r, views.ItemMirrorPanel(vm))
}

// sourceKeyFor looks up a row's source + source_key by local id. Empty strings
// when the row is gone — callers use it only for best-effort panel rebuilds.
func (s *Server) sourceKeyFor(ctx context.Context, id int64) (string, string) {
	items, err := s.svc.DownloadStatus(ctx, &id)
	if err != nil || len(items) != 1 {
		return "", ""
	}
	return items[0].Source, items[0].SourceKey
}

// --- Queue + storage pages ------------------------------------------------

func (s *Server) handleQueuePage(w http.ResponseWriter, r *http.Request) {
	items, _ := s.svc.DownloadStatus(r.Context(), nil)
	render(w, r, views.QueuePage(s.shell(r.Context(), "Queue", "", "", ""), items))
}

func (s *Server) handleQueueRows(w http.ResponseWriter, r *http.Request) {
	items, _ := s.svc.DownloadStatus(r.Context(), nil)
	// compact=1 is the drawer's narrow block layout; default is the full-page table.
	if r.URL.Query().Get("compact") == "1" {
		render(w, r, views.QueueRowsCompact(items))
		return
	}
	render(w, r, views.QueueRows(items))
}

// handleQueueDrawer returns the slide-over queue body (a self-polling block list)
// loaded into #queue-drawer-body when the drawer opens.
func (s *Server) handleQueueDrawer(w http.ResponseWriter, r *http.Request) {
	items, _ := s.svc.DownloadStatus(r.Context(), nil)
	render(w, r, views.QueueDrawerBody(items))
}

// handleQueueCount returns the topbar's live in-flight badge — queued +
// downloading only (an errored row isn't "in flight"), polled every 5s.
func (s *Server) handleQueueCount(w http.ResponseWriter, r *http.Request) {
	items, _ := s.svc.DownloadStatus(r.Context(), nil)
	n := 0
	for _, it := range items {
		if it.Status == "queued" || it.Status == "downloading" {
			n++
		}
	}
	render(w, r, views.QueueCount(n))
}

func (s *Server) handleStoragePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	render(w, r, views.StoragePage(s.shell(ctx, "Storage", "storage", "", ""), s.storageVM(ctx)))
}

func (s *Server) storageVM(ctx context.Context) views.StorageVM {
	vm := views.StorageVM{}
	if stats, err := s.svc.StorageStats(ctx); err != nil {
		vm.Err = err.Error()
	} else {
		vm.Stats = stats
	}
	if items, err := s.svc.ListMirrored(ctx, ""); err != nil {
		if vm.Err == "" {
			vm.Err = err.Error()
		}
	} else {
		vm.Items = items
	}
	return vm
}

// --- Auth (cookie-based shared secret) ------------------------------------

// portalAuth gates the browser portal. With no AuthToken it passes through
// (Traefik forward-auth / trusted LAN). Otherwise it accepts a matching cookie
// or bearer header, and redirects unauthenticated requests to the login page.
func (s *Server) portalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authed(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// authed reports whether a request carries the configured token via the auth
// cookie or an Authorization: Bearer header. Always true when no token is set.
func (s *Server) authed(r *http.Request) bool {
	if s.cfg.AuthToken == "" {
		return true
	}
	want := []byte(s.cfg.AuthToken)
	if c, err := r.Cookie(authCookieName); err == nil &&
		subtle.ConstantTimeCompare([]byte(c.Value), want) == 1 {
		return true
	}
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), want) == 1 {
		return true
	}
	return false
}

func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthToken == "" || s.authed(r) {
		http.Redirect(w, r, "/browse", http.StatusFound)
		return
	}
	render(w, r, views.LoginPage(""))
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthToken == "" {
		http.Redirect(w, r, "/browse", http.StatusSeeOther)
		return
	}
	token := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AuthToken)) != 1 {
		// Set the content type before WriteHeader commits the header block, else
		// the Set inside render is a no-op and the 401 page goes out untyped.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		render(w, r, views.LoginPage("Incorrect token."))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    s.cfg.AuthToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
	http.Redirect(w, r, "/browse", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Small helpers --------------------------------------------------------

// shell builds the PageMeta chrome for a full page: title, active section, the
// auth flag, and the sidebar nav tree (sources → libraries). ListSources is a
// cheap live atomic load; each source's libraries come from the TTL cache so the
// sidebar isn't a fresh network round-trip on every page. activeSource/Library
// mark the rail entry to highlight ("" for non-browse pages).
func (s *Server) shell(ctx context.Context, title, active, activeSource, activeLibrary string) views.PageMeta {
	srcs := s.svc.ListSources()
	// Fetch each source's libraries concurrently so one slow/unreachable source
	// (each bounded by navLibraries' timeout) doesn't serialize page latency. The
	// cache is mutex-guarded and each goroutine handles a distinct source.
	nav := make([]views.NavSource, len(srcs))
	var wg sync.WaitGroup
	for i, si := range srcs {
		wg.Add(1)
		go func(i int, si service.SourceInfo) {
			defer wg.Done()
			libs, errMsg := s.navLibraries(ctx, si.Name)
			nav[i] = views.NavSource{
				Name:         si.Name,
				Downloadable: si.Downloadable,
				Libraries:    libs,
				Err:          errMsg,
			}
		}(i, si)
	}
	wg.Wait()
	return views.PageMeta{
		Title:         title,
		Active:        active,
		AuthEnabled:   s.cfg.AuthToken != "",
		Nav:           nav,
		ActiveSource:  activeSource,
		ActiveLibrary: activeLibrary,
	}
}

// navCache memoizes per-source library lists for the sidebar. Only the
// (network) ListLibraries result is cached; the source set itself stays live via
// ListSources. Cleared on settings reload (handleSettingsSave) since that can
// change which libraries a source exposes.
type navCache struct {
	mu      sync.Mutex
	entries map[string]navCacheEntry
}

type navCacheEntry struct {
	libs    []source.Library
	errMsg  string
	fetched time.Time
}

func (c *navCache) clear() {
	c.mu.Lock()
	c.entries = nil
	c.mu.Unlock()
}

// navLibraries returns a source's libraries for the sidebar, fetching (behind a
// short timeout) on a cache miss/expiry. An unreachable source yields a non-empty
// errMsg the rail renders inline, so one dead source can't hang or break the
// portal. The error is cached too, so a down source isn't retried (and re-hung)
// on every page render for the TTL window.
func (s *Server) navLibraries(ctx context.Context, srcName string) (libs []source.Library, errMsg string) {
	s.nav.mu.Lock()
	if e, ok := s.nav.entries[srcName]; ok && time.Since(e.fetched) < navCacheTTL {
		s.nav.mu.Unlock()
		return e.libs, e.errMsg
	}
	s.nav.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	e := navCacheEntry{fetched: time.Now()}
	if got, err := s.svc.ListLibraries(fctx, srcName); err != nil {
		e.errMsg = "unavailable"
	} else {
		e.libs = got
	}

	s.nav.mu.Lock()
	if s.nav.entries == nil {
		s.nav.entries = map[string]navCacheEntry{}
	}
	s.nav.entries[srcName] = e
	s.nav.mu.Unlock()
	return e.libs, e.errMsg
}

// resolveView picks the browse results render mode: an explicit ?view (which is
// also persisted to a cookie so it sticks across libraries), else the cookie,
// else "card".
func (s *Server) resolveView(w http.ResponseWriter, r *http.Request) string {
	if v := r.URL.Query().Get("view"); v == "card" || v == "table" {
		http.SetCookie(w, &http.Cookie{
			Name: viewCookieName, Value: v, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		return v
	}
	if c, err := r.Cookie(viewCookieName); err == nil && (c.Value == "card" || c.Value == "table") {
		return c.Value
	}
	return "card"
}

// render writes an HTML component, logging (but not double-writing) on failure.
func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		slog.Error("portal: render failed", "path", r.URL.Path, "err", err)
	}
}

// idParam parses the required integer "id" form value, writing a 400 and
// returning ok=false when it's missing or malformed.
func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad or missing id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

func downloadableFor(sources []service.SourceInfo, name string) bool {
	for _, s := range sources {
		if s.Name == name {
			return s.Downloadable
		}
	}
	return false
}

func libraryTitle(libs []source.Library, id string) string {
	for _, l := range libs {
		if l.ID == id {
			return l.Title
		}
	}
	return id
}

// isHTTPS reports whether the original client request was HTTPS, honoring
// Traefik's X-Forwarded-Proto so the auth cookie gets the Secure flag behind
// the reverse proxy.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
