// Package service is the shared core layer that both faces of plex-mirror —
// the web portal (Phase 4) and the MCP server (Phase 5) — call into. It owns
// the configured sources, the storage manager, and the download engine, and
// exposes the mirror operations (browse, queue, status, stats, evict) as plain
// Go methods so neither face re-implements business logic.
//
// Everything here is transport-agnostic: methods take a context and primitive
// arguments and return typed DTOs or a sentinel error. HTTP/MCP shaping lives
// in the caller.
//
// Live reconfiguration (glb-gdl.13): the configurable bits — config, sources,
// engine, storage manager — live in an immutable *runtime swapped atomically by
// Reload. Operation methods take one runtime snapshot at entry (s.now()), so an
// in-flight request always sees a consistent (cfg, sources, engine, storage)
// tuple even while a reload rebuilds the next one. The store and the settings
// store are immutable for the Service's lifetime and stay direct fields.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/download"
	"github.com/maxdubrinsky/plex-mirror/internal/settings"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/source/jellyfin"
	"github.com/maxdubrinsky/plex-mirror/internal/source/plex"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// Sentinel errors callers can branch on. Source-level failures pass through as
// the source package's sentinels (source.ErrAuth, ErrNotFound, ...).
var (
	// ErrSourceNotFound means the requested source name isn't configured.
	ErrSourceNotFound = errors.New("service: source not configured")
	// ErrDownloadUnavailable means no download engine is wired (Plex creds absent).
	ErrDownloadUnavailable = errors.New("service: download not available (Plex not configured)")
	// ErrItemNotFound means no local item row matches the given id.
	ErrItemNotFound = errors.New("service: item not found")
	// ErrChildrenUnavailable means the source can't list an item's children
	// (it doesn't implement source.ChildLister).
	ErrChildrenUnavailable = errors.New("service: source does not support listing children")
)

// runtime is the swappable bundle of everything a config change can rebuild. It
// is treated as immutable once stored: Reload builds a fresh one and swaps the
// pointer rather than mutating fields in place, so readers never see a torn
// state.
type runtime struct {
	cfg     *config.Config
	storage *storage.Manager
	// sources is keyed by Source.Name() — "plex", "jellyfin". Only configured
	// sources are present.
	sources map[string]source.Source
	// engine drives downloads for the Plex source. nil when Plex isn't
	// configured; download ops return ErrDownloadUnavailable in that case.
	engine *download.Engine
}

// source looks up a configured source by name, returning ErrSourceNotFound when
// absent so callers can give a clean "not configured" message.
func (r *runtime) source(name string) (source.Source, error) {
	src, ok := r.sources[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSourceNotFound, name)
	}
	return src, nil
}

// Service is the core layer. Construct with New, optionally call Start to spin
// up background workers (storage sweeper + download daemon), then call the
// operation methods from any face.
type Service struct {
	// baseCfg is the env bootstrap config — the immutable overlay base the
	// settings layer is applied on top of. Never the running config; read the
	// running config from s.now().cfg.
	baseCfg  *config.Config
	store    *db.Store       // immutable
	settings *settings.Store // immutable; DB-backed config overlay

	rt atomic.Pointer[runtime] // the live runtime; swapped by Reload

	// mu serializes Reload and worker lifecycle so two reloads (or a reload and
	// Start) can't interleave worker start/stop.
	mu            sync.Mutex
	runCtx        context.Context    // set by Start; nil before Start (CLI/tests)
	workersCancel context.CancelFunc // cancels the current worker generation
	workersWG     *sync.WaitGroup    // waits out the current worker generation
}

// now returns the current runtime snapshot. Cheap (atomic load); call once at
// the top of an operation and reuse the result so the whole operation sees a
// consistent runtime.
func (s *Service) now() *runtime { return s.rt.Load() }

// SourceInfo describes a configured source for list_sources / the portal's
// source picker.
type SourceInfo struct {
	Name         string `json:"name"`
	Downloadable bool   `json:"downloadable"`
}

// New builds the service from config + an open store. It applies any
// DB-stored settings on top of the env config, then builds the initial runtime
// (storage manager, whichever sources have creds, and a download engine when
// Plex is present). Returns an error only for genuine misconfiguration (bad
// client construction); absent creds are non-fatal and simply leave that source
// / the engine disabled.
func New(cfg *config.Config, store *db.Store) (*Service, error) {
	s := &Service{
		baseCfg:  cfg,
		store:    store,
		settings: settings.NewStore(store, cfg.SecretKey),
	}

	// If a key is now configured, encrypt any secret that was written in
	// plaintext under an earlier no-key run, so adding PLEXMIRROR_SECRET_KEY
	// after the fact actually closes the at-rest exposure.
	ectx, ecancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.settings.EncryptPlaintextSecrets(ectx); err != nil {
		slog.Warn("service: could not re-encrypt plaintext secrets at rest", "err", err)
	}
	ecancel()

	// Overlay DB-stored settings on the env bootstrap. Non-fatal: a load failure
	// or an invalid stored value falls back to the env config so the service
	// still boots (and the operator can fix it from the settings page).
	eff := cfg
	lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
	vals, verr := s.settings.GetAll(lctx)
	lcancel()
	switch {
	case verr != nil:
		slog.Warn("service: could not load stored settings; using env config", "err", verr)
	default:
		if merged, merr := settings.Effective(cfg, vals); merr != nil {
			slog.Warn("service: stored settings invalid; using env config", "err", merr)
		} else {
			eff = merged
		}
	}

	bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bcancel()
	rt, err := buildRuntime(bctx, eff, store)
	if err != nil {
		return nil, err
	}
	s.rt.Store(rt)
	return s, nil
}

// buildRuntime wires a runtime from a (already-merged) config: a storage
// manager, whichever sources have creds, and a download engine when Plex is
// present. Plex discovery failure is non-fatal (Plex stays disabled); absent
// creds simply leave a source / the engine off. Returns an error only for a
// genuine client-construction failure.
func buildRuntime(ctx context.Context, cfg *config.Config, store *db.Store) (*runtime, error) {
	storageMgr := storage.NewManager(store, storage.Policy{
		MediaRoot:    cfg.MediaRoot,
		HardCapBytes: cfg.StorageHardCapBytes,
		SoftCapBytes: cfg.StorageSoftCapBytes,
	})

	rt := &runtime{
		cfg:     cfg,
		storage: storageMgr,
		sources: map[string]source.Source{},
	}

	// Resolve the Plex connection. An explicit PlexURL wins; otherwise, given an
	// account token + server name, discover the connection + per-resource token
	// from plex.tv (so a volatile *.plex.direct URL never has to be hardcoded).
	// Discovery failure is non-fatal: browse/MCP/Jellyfin still boot and Plex
	// simply stays disabled until the next (re)configuration.
	plexURL, plexToken := cfg.PlexURL, cfg.PlexToken
	if plexURL == "" && cfg.PlexServer != "" && plexToken != "" {
		dctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		disc, derr := plex.Discover(dctx, plex.DiscoverOptions{
			Token:            plexToken,
			Server:           cfg.PlexServer,
			ClientIdentifier: cfg.PlexClientID,
		})
		cancel()
		if derr != nil {
			slog.Warn("service: plex discovery failed; Plex disabled until reconfigured",
				"server", cfg.PlexServer, "err", derr)
			plexURL, plexToken = "", ""
		} else {
			plexURL, plexToken = disc.BaseURL, disc.AccessToken
			slog.Info("service: discovered plex server",
				"name", disc.Name, "url", disc.BaseURL, "relay", disc.Relay)
			if disc.Relay {
				slog.Warn("service: chosen Plex connection is a relay (bandwidth-throttled); downloads will be slow")
			}
		}
	}

	if plexURL != "" && plexToken != "" {
		plexSrc, err := plex.New(plex.Config{BaseURL: plexURL, Token: plexToken})
		if err != nil {
			return nil, fmt.Errorf("service: plex client: %w", err)
		}
		rt.sources[plexSrc.Name()] = plexSrc

		resolver, ok := plexSrc.(source.DownloadResolver)
		if !ok {
			return nil, errors.New("service: plex client does not implement DownloadResolver")
		}

		var scanner download.Scanner
		if cfg.JellyfinURL != "" && cfg.JellyfinToken != "" {
			sc, err := jellyfin.NewScanner(jellyfin.Config{
				BaseURL: cfg.JellyfinURL, Token: cfg.JellyfinToken,
			})
			if err != nil {
				return nil, fmt.Errorf("service: jellyfin scanner: %w", err)
			}
			scanner = sc
		}

		engine, err := download.New(store, storageMgr, download.Options{
			MediaRoot:   cfg.MediaRoot,
			MoviesDir:   cfg.MoviesDir,
			ShowsDir:    cfg.ShowsDir,
			OtherDir:    cfg.OtherDir,
			Concurrency: cfg.DownloadConcurrency,
			BufferSize:  int(cfg.DownloadBufferBytes),
			Scanner:     scanner,
			SourceName:  plexSrc.Name(),
			Resolver:    resolver,
		})
		if err != nil {
			return nil, fmt.Errorf("service: download engine: %w", err)
		}
		rt.engine = engine
	}

	if cfg.JellyfinURL != "" && cfg.JellyfinToken != "" {
		jfSrc, err := jellyfin.New(jellyfin.Config{
			BaseURL: cfg.JellyfinURL, Token: cfg.JellyfinToken, User: cfg.JellyfinUser,
		})
		if err != nil {
			return nil, fmt.Errorf("service: jellyfin client: %w", err)
		}
		rt.sources[jfSrc.Name()] = jfSrc
	}

	if rt.engine == nil {
		slog.Info("service: download engine disabled (no Plex URL/token); browse + MCP still available")
	}
	return rt, nil
}

// Start launches background workers and returns immediately. They stop when ctx
// is cancelled. Reload cycles them onto a fresh runtime without touching ctx. A
// zero sweep / poll interval disables that worker (the manager and engine handle
// that internally). Safe to skip in CLI/test contexts that drive operations
// synchronously.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runCtx = ctx
	// Recover any row a prior process left stranded in 'downloading' (crash
	// mid-download). The engine's own ResetStaleDownloads also covers this when an
	// engine starts, but doing it here makes recovery independent of whether an
	// engine exists or the poll loop is enabled.
	s.resetStaleDownloads(ctx)
	s.launchWorkersLocked(s.now())
}

// launchWorkersLocked starts the sweeper + download daemon for rt under a child
// of runCtx, recording the cancel + waitgroup so a later Reload (or a repeat
// Start) can stop this generation. It first tears down any existing generation,
// so it is safe to call more than once. No-op before Start (runCtx nil): CLI/test
// callers drive operations synchronously and never want background workers.
//
// Caller must hold s.mu.
func (s *Service) launchWorkersLocked(rt *runtime) {
	if s.runCtx == nil {
		return
	}
	s.stopWorkersLocked()
	wctx, cancel := context.WithCancel(s.runCtx)
	wg := &sync.WaitGroup{}

	wg.Go(func() {
		rt.storage.RunSweeper(wctx, rt.cfg.StorageSweepEvery)
	})
	if rt.engine != nil {
		wg.Go(func() {
			rt.engine.Run(wctx, rt.cfg.DownloadPollEvery)
		})
	}

	s.workersCancel = cancel
	s.workersWG = wg
}

// stopWorkersLocked cancels and waits out the current worker generation, if any.
// Caller must hold s.mu.
func (s *Service) stopWorkersLocked() {
	if s.workersCancel != nil {
		s.workersCancel()
		s.workersWG.Wait()
		s.workersCancel = nil
		s.workersWG = nil
	}
}

// resetStaleDownloads flips any row left in 'downloading' back to 'queued'. Only
// the (single) engine ever sets 'downloading', so once the workers are stopped
// any such row is genuinely orphaned. Engine-independent (plain SQL) and
// best-effort — it recovers a download abandoned by a reload even when the new
// runtime has no engine or a disabled poll loop, which the engine's own
// ResetStaleDownloads (it runs only when an engine starts) would miss.
func (s *Service) resetStaleDownloads(ctx context.Context) {
	if _, err := s.store.ExecContext(ctx,
		`UPDATE items SET status = 'queued' WHERE status = 'downloading'`); err != nil {
		slog.Warn("service: reset stale downloads failed", "err", err)
	}
}

// Reload rebuilds the runtime from the current env+DB settings and swaps it in
// live, with no process restart (glb-gdl.13). It stops the previous worker
// generation and waits it out BEFORE swapping, so the old engine is no longer
// writing partials when the new one starts (they share the deterministic
// .partials paths). An in-flight download abandoned this way leaves a consistent
// partial on disk and its row stuck in 'downloading'; resetStaleDownloads (run
// here, engine-independent) flips it back to 'queued' so it resumes from the
// on-disk offset — even if the new runtime has no engine or downloads were
// disabled.
//
// On any build/validate failure the current runtime and its workers are left
// untouched and the error is returned.
func (s *Service) Reload(ctx context.Context) error {
	vals, err := s.settings.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("reload: load settings: %w", err)
	}
	eff, err := settings.Effective(s.baseCfg, vals)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	newRT, err := buildRuntime(ctx, eff, s.store)
	if err != nil {
		return fmt.Errorf("reload: build runtime: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopWorkersLocked()      // stop the old generation before anything else touches state
	s.resetStaleDownloads(ctx) // recover a download it abandoned mid-flight
	s.rt.Store(newRT)          // publish the new runtime to readers
	s.launchWorkersLocked(newRT)
	slog.Info("service: reloaded configuration live", "sources", len(newRT.sources),
		"downloads_enabled", newRT.engine != nil)
	return nil
}

// ListSources reports the configured sources and whether each can be downloaded
// from (only Plex resolves download URLs today). Ordered by name for stable
// output.
func (s *Service) ListSources() []SourceInfo {
	rt := s.now()
	out := make([]SourceInfo, 0, len(rt.sources))
	for name, src := range rt.sources {
		_, downloadable := src.(source.DownloadResolver)
		out = append(out, SourceInfo{Name: name, Downloadable: downloadable})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListLibraries returns the libraries on a source.
func (s *Service) ListLibraries(ctx context.Context, sourceName string) ([]source.Library, error) {
	src, err := s.now().source(sourceName)
	if err != nil {
		return nil, err
	}
	return src.ListLibraries(ctx)
}

// ListItems lists items in a library. filter, when non-empty, is a title
// search pushed to the adapter (source.ListOptions.Query) so it spans the whole
// library rather than just the requested page. We deliberately do not re-filter
// client-side: backends do fuzzy/token matching (Jellyfin SearchTerm, Plex
// per-word prefix), and a naive substring pass would wrongly drop legitimate
// matches like "lord rings" -> "The Lord of the Rings". limit/offset page the
// result and are passed through to the adapter.
func (s *Service) ListItems(ctx context.Context, sourceName, libraryID, filter string, limit, offset int) ([]source.Item, error) {
	src, err := s.now().source(sourceName)
	if err != nil {
		return nil, err
	}
	return src.ListItems(ctx, libraryID, source.ListOptions{Offset: offset, Limit: limit, Query: filter})
}

// ListChildren returns the direct children of a container item (show→seasons,
// season→episodes). It returns ErrChildrenUnavailable if the source can't
// traverse a hierarchy (doesn't implement source.ChildLister).
func (s *Service) ListChildren(ctx context.Context, sourceName, itemID string, limit, offset int) ([]source.Item, error) {
	src, err := s.now().source(sourceName)
	if err != nil {
		return nil, err
	}
	lister, ok := src.(source.ChildLister)
	if !ok {
		return nil, ErrChildrenUnavailable
	}
	return lister.ListChildren(ctx, itemID, source.ListOptions{Offset: offset, Limit: limit})
}

// GetItem returns metadata for a single item on a source.
func (s *Service) GetItem(ctx context.Context, sourceName, itemID string) (source.Item, error) {
	src, err := s.now().source(sourceName)
	if err != nil {
		return source.Item{}, err
	}
	return src.GetMetadata(ctx, itemID)
}
