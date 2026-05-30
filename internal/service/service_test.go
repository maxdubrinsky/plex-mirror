package service

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/download"
	"github.com/maxdubrinsky/plex-mirror/internal/settings"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// browseSource is a source.Source stub. downloadSource adds the optional
// DownloadResolver capability so we can exercise the Plex-only paths.
type browseSource struct {
	name  string
	libs  []source.Library
	items map[string][]source.Item
	meta  map[string]source.Item
	err   error
}

func (s *browseSource) Name() string { return s.name }

func (s *browseSource) ListLibraries(context.Context) ([]source.Library, error) {
	return s.libs, s.err
}

func (s *browseSource) ListItems(_ context.Context, libraryID string, opts source.ListOptions) ([]source.Item, error) {
	if s.err != nil {
		return nil, s.err
	}
	items := s.items[libraryID]
	// Honor opts.Query like a real backend's server-side title search; the
	// service no longer re-filters client-side, so this is where search happens.
	if q := strings.ToLower(strings.TrimSpace(opts.Query)); q != "" {
		matched := make([]source.Item, 0, len(items))
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.Title), q) {
				matched = append(matched, it)
			}
		}
		items = matched
	}
	return items, nil
}

func (s *browseSource) GetMetadata(_ context.Context, itemID string) (source.Item, error) {
	if s.err != nil {
		return source.Item{}, s.err
	}
	it, ok := s.meta[itemID]
	if !ok {
		return source.Item{}, source.ErrNotFound
	}
	return it, nil
}

type downloadSource struct {
	*browseSource
}

func (d *downloadSource) ResolveDownloadURL(context.Context, string) (*source.DownloadTarget, error) {
	return &source.DownloadTarget{URL: "http://example.test/file"}, nil
}

// childSource adds the optional source.ChildLister capability for hierarchy
// traversal (show→season→episode).
type childSource struct {
	*browseSource
	children map[string][]source.Item
}

func (c *childSource) ListChildren(_ context.Context, itemID string, _ source.ListOptions) ([]source.Item, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.children[itemID], nil
}

func buildService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	cfg := &config.Config{MediaRoot: root}
	storageMgr := storage.NewManager(store, storage.Policy{MediaRoot: root})

	plexStub := &downloadSource{browseSource: &browseSource{
		name: "plex",
		libs: []source.Library{{ID: "1", Title: "Movies", Kind: source.LibraryMovies}},
		items: map[string][]source.Item{
			"1": {
				{ID: "42", Title: "Encanto", Kind: source.ItemMovie, Container: "mkv", Year: 2021},
				{ID: "7", Title: "Up", Kind: source.ItemMovie, Container: "mkv", Year: 2009},
			},
		},
		meta: map[string]source.Item{
			"42": {ID: "42", Title: "Encanto", Kind: source.ItemMovie, Container: "mkv", Year: 2021},
		},
	}}
	jellyStub := &childSource{
		browseSource: &browseSource{
			name: "jellyfin",
			libs: []source.Library{{ID: "j1", Title: "Shows", Kind: source.LibraryShows}},
		},
		children: map[string][]source.Item{
			"show-1":   {{ID: "season-1", Title: "Season 1", Kind: source.ItemSeason}},
			"season-1": {{ID: "ep-1", Title: "Pilot", Kind: source.ItemEpisode, Container: "mkv"}},
		},
	}

	engine, err := download.New(store, storageMgr, download.Options{
		MediaRoot:  root,
		SourceName: "plex",
		Resolver:   plexStub,
	})
	if err != nil {
		t.Fatalf("download.New: %v", err)
	}

	return newWithRuntime(store, &runtime{
		cfg:     cfg,
		storage: storageMgr,
		sources: map[string]source.Source{"plex": plexStub, "jellyfin": jellyStub},
		engine:  engine,
	})
}

// newWithRuntime builds a Service around a hand-made runtime, skipping the
// env→settings→discovery path New() runs. Used by the service-package tests that
// wire stub sources directly. runCtx stays nil so no background workers start.
func newWithRuntime(store *db.Store, rt *runtime) *Service {
	s := &Service{
		baseCfg:     rt.cfg,
		store:       store,
		settings:    settings.NewStore(store, rt.cfg.SecretKey),
		health:      map[string]sourceHealth{},
		healthRand:  rand.New(rand.NewSource(1)),
		reconnectCh: make(chan string, 1),
		kickCh:      make(chan struct{}, 1),
	}
	s.rt.Store(rt)
	return s
}

// withoutEngine returns a copy of the service's runtime with the download engine
// disabled, to exercise the "Plex not configured" paths.
func withoutEngine(s *Service) {
	rt := *s.now()
	rt.engine = nil
	s.rt.Store(&rt)
}

func mustFile(t *testing.T, root, name string, size int64) string {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return full
}

func TestListSources(t *testing.T) {
	svc := buildService(t)
	got := svc.ListSources()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	// Sorted by name: jellyfin (browse-only) then plex (downloadable).
	if got[0].Name != "jellyfin" || got[0].Downloadable {
		t.Errorf("got[0] = %+v, want {jellyfin false}", got[0])
	}
	if got[1].Name != "plex" || !got[1].Downloadable {
		t.Errorf("got[1] = %+v, want {plex true}", got[1])
	}
}

func TestListLibraries(t *testing.T) {
	svc := buildService(t)
	ctx := context.Background()

	libs, err := svc.ListLibraries(ctx, "plex")
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	if len(libs) != 1 || libs[0].Title != "Movies" {
		t.Fatalf("libs = %+v, want one Movies library", libs)
	}

	if _, err := svc.ListLibraries(ctx, "nope"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("unknown source err = %v, want ErrSourceNotFound", err)
	}
}

func TestListItemsFilter(t *testing.T) {
	svc := buildService(t)
	ctx := context.Background()

	all, err := svc.ListItems(ctx, "plex", "1", "", 50, 0)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered len = %d, want 2", len(all))
	}

	enc, err := svc.ListItems(ctx, "plex", "1", "enc", 50, 0)
	if err != nil {
		t.Fatalf("ListItems filtered: %v", err)
	}
	if len(enc) != 1 || enc[0].Title != "Encanto" {
		t.Fatalf("filtered = %+v, want only Encanto", enc)
	}

	none, err := svc.ListItems(ctx, "plex", "1", "zzz", 50, 0)
	if err != nil {
		t.Fatalf("ListItems no-match: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("no-match len = %d, want 0", len(none))
	}
}

func TestListChildren(t *testing.T) {
	svc := buildService(t)
	ctx := context.Background()

	// jellyfin implements ChildLister: drill show -> season -> episode.
	seasons, err := svc.ListChildren(ctx, "jellyfin", "show-1", 0, 0)
	if err != nil {
		t.Fatalf("ListChildren(show): %v", err)
	}
	if len(seasons) != 1 || seasons[0].ID != "season-1" || seasons[0].Kind != source.ItemSeason {
		t.Fatalf("seasons = %+v, want one season-1", seasons)
	}
	eps, err := svc.ListChildren(ctx, "jellyfin", "season-1", 0, 0)
	if err != nil {
		t.Fatalf("ListChildren(season): %v", err)
	}
	if len(eps) != 1 || eps[0].Kind != source.ItemEpisode || eps[0].Container != "mkv" {
		t.Fatalf("episodes = %+v, want one downloadable episode", eps)
	}

	// plex stub doesn't implement ChildLister -> graceful sentinel.
	if _, err := svc.ListChildren(ctx, "plex", "42", 0, 0); !errors.Is(err, ErrChildrenUnavailable) {
		t.Fatalf("err = %v, want ErrChildrenUnavailable", err)
	}

	// unknown source still maps to ErrSourceNotFound.
	if _, err := svc.ListChildren(ctx, "nope", "x", 0, 0); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("err = %v, want ErrSourceNotFound", err)
	}
}

func TestGetItem(t *testing.T) {
	svc := buildService(t)
	ctx := context.Background()

	it, err := svc.GetItem(ctx, "plex", "42")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if it.Title != "Encanto" {
		t.Fatalf("title = %q, want Encanto", it.Title)
	}

	if _, err := svc.GetItem(ctx, "plex", "999"); !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("missing item err = %v, want source.ErrNotFound", err)
	}
}

func TestQueueDownload(t *testing.T) {
	svc := buildService(t)
	ctx := context.Background()

	it, err := svc.QueueDownload(ctx, "plex", "42")
	if err != nil {
		t.Fatalf("QueueDownload: %v", err)
	}
	if it.ID == 0 || it.SourceKey != "42" || it.Title != "Encanto" || it.Status != "queued" {
		t.Fatalf("queued item = %+v, want id>0 key=42 title=Encanto status=queued", it)
	}

	// Single-item status.
	single, err := svc.DownloadStatus(ctx, &it.ID)
	if err != nil {
		t.Fatalf("DownloadStatus single: %v", err)
	}
	if len(single) != 1 || single[0].ID != it.ID {
		t.Fatalf("single status = %+v, want one row id=%d", single, it.ID)
	}

	// All in-flight.
	all, err := svc.DownloadStatus(ctx, nil)
	if err != nil {
		t.Fatalf("DownloadStatus all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("in-flight len = %d, want 1", len(all))
	}

	// Jellyfin has no resolver → unsupported.
	if _, err := svc.QueueDownload(ctx, "jellyfin", "x"); !errors.Is(err, source.ErrUnsupported) {
		t.Fatalf("jellyfin queue err = %v, want source.ErrUnsupported", err)
	}
	// Unknown source.
	if _, err := svc.QueueDownload(ctx, "nope", "x"); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("unknown source queue err = %v, want ErrSourceNotFound", err)
	}
}

func TestQueueDownloadUnavailable(t *testing.T) {
	svc := buildService(t)
	withoutEngine(svc) // simulate Plex-not-configured
	if _, err := svc.QueueDownload(context.Background(), "plex", "42"); !errors.Is(err, ErrDownloadUnavailable) {
		t.Fatalf("err = %v, want ErrDownloadUnavailable", err)
	}
}

func TestListMirroredAndStorageStats(t *testing.T) {
	svc := buildService(t)
	ctx := context.Background()
	root := svc.now().cfg.MediaRoot

	p1 := mustFile(t, root, "movies/Encanto (2021).mkv", 1000)
	if _, err := svc.now().storage.RecordReady(ctx, storage.ReadyItem{
		Source: "plex", SourceKey: "42", Title: "Encanto", Container: "mkv",
		LocalPath: p1, SizeBytes: 1000,
	}); err != nil {
		t.Fatalf("RecordReady 1: %v", err)
	}
	p2 := mustFile(t, root, "movies/Up (2009).mkv", 500)
	if _, err := svc.now().storage.RecordReady(ctx, storage.ReadyItem{
		Source: "plex", SourceKey: "7", Title: "Up", Container: "mkv",
		LocalPath: p2, SizeBytes: 500,
	}); err != nil {
		t.Fatalf("RecordReady 2: %v", err)
	}

	mirrored, err := svc.ListMirrored(ctx, "")
	if err != nil {
		t.Fatalf("ListMirrored: %v", err)
	}
	if len(mirrored) != 2 {
		t.Fatalf("mirrored len = %d, want 2", len(mirrored))
	}

	enc, err := svc.ListMirrored(ctx, "enc")
	if err != nil {
		t.Fatalf("ListMirrored filtered: %v", err)
	}
	if len(enc) != 1 || enc[0].Title != "Encanto" {
		t.Fatalf("filtered mirrored = %+v, want only Encanto", enc)
	}

	stats, err := svc.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats: %v", err)
	}
	if stats.UsedBytes != 1500 {
		t.Errorf("used = %d, want 1500", stats.UsedBytes)
	}
	if stats.ItemsReady != 2 {
		t.Errorf("ready = %d, want 2", stats.ItemsReady)
	}
	if stats.MediaRoot != root {
		t.Errorf("media root = %q, want %q", stats.MediaRoot, root)
	}
	if stats.FreeBytes <= 0 {
		t.Errorf("free = %d, want > 0", stats.FreeBytes)
	}
}

func TestEvict(t *testing.T) {
	svc := buildService(t)
	ctx := context.Background()
	root := svc.now().cfg.MediaRoot

	p := mustFile(t, root, "movies/Encanto (2021).mkv", 1000)
	id, err := svc.now().storage.RecordReady(ctx, storage.ReadyItem{
		Source: "plex", SourceKey: "42", Title: "Encanto", Container: "mkv",
		LocalPath: p, SizeBytes: 1000,
	})
	if err != nil {
		t.Fatalf("RecordReady: %v", err)
	}

	ev, err := svc.Evict(ctx, id)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if ev.ID != id {
		t.Fatalf("evicted id = %d, want %d", ev.ID, id)
	}
	if _, statErr := os.Stat(p); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("file still present: %v", statErr)
	}

	it, err := svc.itemByID(ctx, id)
	if err != nil {
		t.Fatalf("itemByID: %v", err)
	}
	if it.Status != "evicted" {
		t.Fatalf("status = %q, want evicted", it.Status)
	}

	if _, err := svc.Evict(ctx, 9999); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("missing evict err = %v, want ErrItemNotFound", err)
	}
}

func TestDownloadStatusNotFound(t *testing.T) {
	svc := buildService(t)
	missing := int64(9999)
	if _, err := svc.DownloadStatus(context.Background(), &missing); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}
