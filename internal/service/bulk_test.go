package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/download"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// bulkSource is a downloadable source WITH a show→season→episode hierarchy, so
// it exercises the recursive bulk-queue gather (the standard test stubs keep
// DownloadResolver and ChildLister on separate sources on purpose).
type bulkSource struct {
	meta     map[string]source.Item
	children map[string][]source.Item
}

func (b *bulkSource) Name() string                                            { return "plex" }
func (b *bulkSource) ListLibraries(context.Context) ([]source.Library, error) { return nil, nil }
func (b *bulkSource) ListItems(context.Context, string, source.ListOptions) ([]source.Item, error) {
	return nil, nil
}
func (b *bulkSource) GetMetadata(_ context.Context, id string) (source.Item, error) {
	it, ok := b.meta[id]
	if !ok {
		return source.Item{}, source.ErrNotFound
	}
	return it, nil
}
func (b *bulkSource) ResolveDownloadURL(context.Context, string) (*source.DownloadTarget, error) {
	return &source.DownloadTarget{URL: "http://example.test/file"}, nil
}
func (b *bulkSource) ListChildren(_ context.Context, id string, _ source.ListOptions) ([]source.Item, error) {
	return b.children[id], nil
}

func episode(id, show string, season, ep int) source.Item {
	return source.Item{
		ID: id, Title: show, Kind: source.ItemEpisode, Container: "mkv",
		ShowTitle: show, SeasonNumber: season, EpisodeNumber: ep, SizeBytes: 1000,
	}
}

func buildBulkService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Hierarchy: show "sh" → seasons s1,s2 → s1:{e1,e2} s2:{e3}.
	src := &bulkSource{
		meta: map[string]source.Item{
			"sh": {ID: "sh", Title: "Show", Kind: source.ItemShow},
			"s1": {ID: "s1", Title: "Season 1", Kind: source.ItemSeason, ParentID: "sh"},
			"s2": {ID: "s2", Title: "Season 2", Kind: source.ItemSeason, ParentID: "sh"},
			"e1": episode("e1", "Show", 1, 1),
			"e2": episode("e2", "Show", 1, 2),
			"e3": episode("e3", "Show", 2, 1),
		},
		children: map[string][]source.Item{
			"sh": {{ID: "s1", Title: "Season 1", Kind: source.ItemSeason, ParentID: "sh"}, {ID: "s2", Title: "Season 2", Kind: source.ItemSeason, ParentID: "sh"}},
			"s1": {episode("e1", "Show", 1, 1), episode("e2", "Show", 1, 2)},
			"s2": {episode("e3", "Show", 2, 1)},
		},
	}
	// episodes carry ParentID for the season-span count in PreviewContainer.
	for _, id := range []string{"e1", "e2"} {
		it := src.meta[id]
		it.ParentID = "s1"
		src.meta[id] = it
	}
	e3 := src.meta["e3"]
	e3.ParentID = "s2"
	src.meta["e3"] = e3
	for i := range src.children["s1"] {
		src.children["s1"][i].ParentID = "s1"
	}
	src.children["s2"][0].ParentID = "s2"

	storageMgr := storage.NewManager(store, storage.Policy{MediaRoot: root})
	engine, err := download.New(store, storageMgr, download.Options{
		MediaRoot: root, SourceName: "plex", Resolver: src,
	})
	if err != nil {
		t.Fatalf("download.New: %v", err)
	}
	return newWithRuntime(store, &runtime{
		cfg:     &config.Config{MediaRoot: root},
		storage: storageMgr,
		sources: map[string]source.Source{"plex": src},
		engine:  engine,
	})
}

func TestQueueContainerSeason(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	res, err := svc.QueueContainer(ctx, "plex", "s1")
	if err != nil {
		t.Fatalf("QueueContainer season: %v", err)
	}
	if res.Queued != 2 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("season result = %+v, want queued=2 skipped=0 failed=0", res)
	}
	if res.TotalBytes != 2000 {
		t.Errorf("TotalBytes = %d, want 2000", res.TotalBytes)
	}

	inflight, err := svc.DownloadStatus(ctx, nil)
	if err != nil {
		t.Fatalf("DownloadStatus: %v", err)
	}
	if len(inflight) != 2 {
		t.Fatalf("in-flight = %d, want 2 queued episodes", len(inflight))
	}

	// Idempotent: a second pass queues nothing new (both already queued).
	res2, err := svc.QueueContainer(ctx, "plex", "s1")
	if err != nil {
		t.Fatalf("QueueContainer re-run: %v", err)
	}
	if res2.Skipped != 2 || res2.Queued != 0 {
		t.Fatalf("re-run result = %+v, want skipped=2 queued=0 (already queued)", res2)
	}
	again, _ := svc.DownloadStatus(ctx, nil)
	if len(again) != 2 {
		t.Fatalf("after re-run in-flight = %d, want still 2 (no duplicates)", len(again))
	}
}

func TestQueueContainerShowDescendsSeasons(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	res, err := svc.QueueContainer(ctx, "plex", "sh")
	if err != nil {
		t.Fatalf("QueueContainer show: %v", err)
	}
	if res.Queued != 3 || res.Failed != 0 {
		t.Fatalf("show result = %+v, want queued=3 (all episodes across both seasons)", res)
	}
	if res.Container != "Show" {
		t.Errorf("Container = %q, want Show", res.Container)
	}
	inflight, _ := svc.DownloadStatus(ctx, nil)
	if len(inflight) != 3 {
		t.Fatalf("in-flight = %d, want 3", len(inflight))
	}
}

func TestPreviewContainerCountsAndSeasons(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	// Pre-mirror one episode so it counts as AlreadyHave, not ToQueue.
	root := svc.now().cfg.MediaRoot
	mustFile(t, root, "shows/Show/Season 01/e1.mkv", 1000)
	if _, err := svc.now().storage.RecordReady(ctx, storage.ReadyItem{
		Source: "plex", SourceKey: "e1", Title: "Show", Container: "mkv",
		LocalPath: filepath.Join(root, "shows/Show/Season 01/e1.mkv"), SizeBytes: 1000,
	}); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}

	prev, err := svc.PreviewContainer(ctx, "plex", "sh")
	if err != nil {
		t.Fatalf("PreviewContainer: %v", err)
	}
	if prev.ToQueue != 2 || prev.AlreadyHave != 1 {
		t.Fatalf("preview = %+v, want toQueue=2 alreadyHave=1", prev)
	}
	if prev.Seasons != 2 {
		t.Errorf("Seasons = %d, want 2 (episodes span s1+s2)", prev.Seasons)
	}
	if prev.TotalBytes != 2000 {
		t.Errorf("TotalBytes = %d, want 2000 (only the to-queue leaves)", prev.TotalBytes)
	}
}

func TestQueueContainerErrors(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	// Unknown source.
	if _, err := svc.QueueContainer(ctx, "nope", "sh"); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("unknown source err = %v, want ErrSourceNotFound", err)
	}
	// Engine disabled.
	withoutEngine(svc)
	if _, err := svc.QueueContainer(ctx, "plex", "sh"); !errors.Is(err, ErrDownloadUnavailable) {
		t.Errorf("no-engine err = %v, want ErrDownloadUnavailable", err)
	}
}

func TestQueueContainerNonDownloadableSource(t *testing.T) {
	// A source that lists children but can't download → ErrUnsupported.
	svc := buildBulkService(t)
	ctx := context.Background()
	svc.now().sources["browseonly"] = &childSource{
		browseSource: &browseSource{name: "browseonly"},
		children:     map[string][]source.Item{},
	}
	if _, err := svc.QueueContainer(ctx, "browseonly", "x"); !errors.Is(err, source.ErrUnsupported) {
		t.Errorf("err = %v, want source.ErrUnsupported", err)
	}
}
