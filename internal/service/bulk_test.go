package service

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// mirrorReady records an episode as a ready local mirror (file on disk + DB row),
// so the bulk-evict tests have something to remove.
func mirrorReady(t *testing.T, svc *Service, key string, season int, size int64) string {
	t.Helper()
	root := svc.now().cfg.MediaRoot
	rel := fmt.Sprintf("shows/Show/Season %02d/%s.mkv", season, key)
	path := mustFile(t, root, rel, size)
	if _, err := svc.now().storage.RecordReady(context.Background(), storage.ReadyItem{
		Source: "plex", SourceKey: key, Title: "Show", Container: "mkv",
		LocalPath: path, SizeBytes: size,
	}); err != nil {
		t.Fatalf("RecordReady %s: %v", key, err)
	}
	return path
}

// readyKeys returns the source_keys of every row currently in 'ready' state, so a
// test can assert exactly which mirrors survived an eviction.
func readyKeys(t *testing.T, svc *Service) map[string]bool {
	t.Helper()
	mirrored, err := svc.ListMirrored(context.Background(), "")
	if err != nil {
		t.Fatalf("ListMirrored: %v", err)
	}
	out := map[string]bool{}
	for _, m := range mirrored {
		out[m.SourceKey] = true
	}
	return out
}

func TestEvictContainerSeason(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	// Mirror both season-1 episodes and the unrelated season-2 episode.
	p1 := mirrorReady(t, svc, "e1", 1, 1000)
	mirrorReady(t, svc, "e2", 1, 1000)
	mirrorReady(t, svc, "e3", 2, 1000)

	res, err := svc.EvictContainer(ctx, "plex", "s1")
	if err != nil {
		t.Fatalf("EvictContainer season: %v", err)
	}
	if res.Evicted != 2 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("season result = %+v, want evicted=2 skipped=0 failed=0", res)
	}
	if res.FreedBytes != 2000 {
		t.Errorf("FreedBytes = %d, want 2000", res.FreedBytes)
	}
	// The season-1 files are gone; season 2 is untouched.
	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Errorf("e1 file still present after eviction (stat err = %v)", err)
	}
	survivors := readyKeys(t, svc)
	if survivors["e1"] || survivors["e2"] {
		t.Errorf("season-1 episodes still ready: %v", survivors)
	}
	if !survivors["e3"] {
		t.Errorf("season-2 episode should be untouched, ready set = %v", survivors)
	}

	// Idempotent: a second pass finds nothing left to evict.
	res2, err := svc.EvictContainer(ctx, "plex", "s1")
	if err != nil {
		t.Fatalf("EvictContainer re-run: %v", err)
	}
	if res2.Evicted != 0 || res2.Skipped != 2 {
		t.Fatalf("re-run result = %+v, want evicted=0 skipped=2", res2)
	}
}

func TestEvictContainerShowDescendsSeasons(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	mirrorReady(t, svc, "e1", 1, 1000)
	mirrorReady(t, svc, "e2", 1, 1000)
	mirrorReady(t, svc, "e3", 2, 1000)

	res, err := svc.EvictContainer(ctx, "plex", "sh")
	if err != nil {
		t.Fatalf("EvictContainer show: %v", err)
	}
	if res.Evicted != 3 || res.Failed != 0 {
		t.Fatalf("show result = %+v, want evicted=3 (all episodes across both seasons)", res)
	}
	if res.Container != "Show" {
		t.Errorf("Container = %q, want Show", res.Container)
	}
	if res.FreedBytes != 3000 {
		t.Errorf("FreedBytes = %d, want 3000", res.FreedBytes)
	}
	if len(readyKeys(t, svc)) != 0 {
		t.Errorf("expected no ready mirrors left, got %v", readyKeys(t, svc))
	}
}

func TestEvictContainerSingleEpisode(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()
	mirrorReady(t, svc, "e1", 1, 1000)
	mirrorReady(t, svc, "e2", 1, 1000)

	// Pointing EvictContainer at a leaf evicts just that one (gatherLeaves returns
	// the root itself when it carries a file).
	res, err := svc.EvictContainer(ctx, "plex", "e1")
	if err != nil {
		t.Fatalf("EvictContainer episode: %v", err)
	}
	if res.Evicted != 1 {
		t.Fatalf("episode result = %+v, want evicted=1", res)
	}
	if survivors := readyKeys(t, svc); survivors["e1"] || !survivors["e2"] {
		t.Errorf("ready set = %v, want only e2", survivors)
	}
}

func TestEvictContainerSkipsUnmirrored(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	// Nothing mirrored: every leaf is skipped, nothing fails.
	res, err := svc.EvictContainer(ctx, "plex", "sh")
	if err != nil {
		t.Fatalf("EvictContainer: %v", err)
	}
	if res.Evicted != 0 || res.Failed != 0 || res.Skipped != 3 {
		t.Fatalf("result = %+v, want evicted=0 failed=0 skipped=3", res)
	}
}

func TestPreviewEvictCountsAndSeasons(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	// Mirror one of the two seasons fully + one episode of the other.
	mirrorReady(t, svc, "e1", 1, 1000)
	mirrorReady(t, svc, "e3", 2, 1000)

	prev, err := svc.PreviewEvict(ctx, "plex", "sh")
	if err != nil {
		t.Fatalf("PreviewEvict: %v", err)
	}
	if prev.ToEvict != 2 {
		t.Fatalf("preview = %+v, want toEvict=2 (e1+e3)", prev)
	}
	if prev.FreedBytes != 2000 {
		t.Errorf("FreedBytes = %d, want 2000", prev.FreedBytes)
	}
	if prev.Seasons != 2 {
		t.Errorf("Seasons = %d, want 2 (e1 in s1, e3 in s2)", prev.Seasons)
	}
	if prev.Kind != "show" {
		t.Errorf("Kind = %q, want show", prev.Kind)
	}
}

func TestEvictContainerErrors(t *testing.T) {
	svc := buildBulkService(t)
	ctx := context.Background()

	// Unknown source.
	if _, err := svc.EvictContainer(ctx, "nope", "sh"); !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("unknown source err = %v, want ErrSourceNotFound", err)
	}
	// A browse-only source that can't traverse a hierarchy → ErrChildrenUnavailable
	// (note: unlike queue, evict does NOT require a downloadable source).
	svc.now().sources["browseonly"] = &browseSource{name: "browseonly", meta: map[string]source.Item{
		"sh": {ID: "sh", Title: "Show", Kind: source.ItemShow},
	}}
	if _, err := svc.EvictContainer(ctx, "browseonly", "sh"); !errors.Is(err, ErrChildrenUnavailable) {
		t.Errorf("browse-only err = %v, want ErrChildrenUnavailable", err)
	}
}
