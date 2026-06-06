package views

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/maxdubrinsky/plex-mirror/internal/service"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

func renderComponent(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func TestBreadcrumbTrailEpisode(t *testing.T) {
	vm := ItemDetailVM{
		Source: "plex",
		Item: source.Item{
			ID: "114205", Title: "Wolferton Splash", Kind: source.ItemEpisode,
			ShowTitle: "The Crown", GrandparentID: "114203",
			ParentTitle: "Season 1", ParentID: "114204",
			LibraryID: "2", LibraryTitle: "TV Shows",
		},
	}
	trail := vm.breadcrumbTrail()
	if len(trail) != 3 {
		t.Fatalf("trail len = %d, want 3 (library, show, season): %+v", len(trail), trail)
	}
	wantLabels := []string{"TV Shows", "The Crown", "Season 1"}
	for i, c := range trail {
		if c.Label != wantLabels[i] {
			t.Errorf("crumb[%d].Label = %q, want %q", i, c.Label, wantLabels[i])
		}
	}
	if got := string(trail[0].Href); !strings.Contains(got, "library=2") || !strings.Contains(got, "source=plex") {
		t.Errorf("library crumb href = %q, want /browse with library=2&source=plex", got)
	}
	if got := string(trail[1].Href); !strings.Contains(got, "id=114203") {
		t.Errorf("show crumb href = %q, want /item id=114203", got)
	}
	if got := string(trail[2].Href); !strings.Contains(got, "id=114204") {
		t.Errorf("season crumb href = %q, want /item id=114204", got)
	}
}

func TestBreadcrumbTrailSeason(t *testing.T) {
	vm := ItemDetailVM{
		Source: "plex",
		Item: source.Item{
			ID: "114204", Title: "Season 1", Kind: source.ItemSeason,
			ParentTitle: "The Crown", ParentID: "114203",
			LibraryID: "2", LibraryTitle: "TV Shows",
		},
	}
	trail := vm.breadcrumbTrail()
	if len(trail) != 2 {
		t.Fatalf("trail len = %d, want 2 (library, show): %+v", len(trail), trail)
	}
	if trail[1].Label != "The Crown" || !strings.Contains(string(trail[1].Href), "id=114203") {
		t.Errorf("show crumb = %+v, want The Crown -> id=114203", trail[1])
	}
}

func TestBreadcrumbTrailMovieRootOnly(t *testing.T) {
	// No library id → root falls back to a generic "Browse" link, no ancestors.
	vm := ItemDetailVM{
		Source: "plex",
		Item:   source.Item{ID: "1001", Title: "Solaris", Kind: source.ItemMovie},
	}
	trail := vm.breadcrumbTrail()
	if len(trail) != 1 {
		t.Fatalf("movie trail len = %d, want 1 (root only): %+v", len(trail), trail)
	}
	if trail[0].Label != "Browse" {
		t.Errorf("root label = %q, want Browse", trail[0].Label)
	}
}

// The rendered breadcrumb must emit clickable links up to the season and show.
func TestBreadcrumbRendersLinks(t *testing.T) {
	vm := ItemDetailVM{
		Source: "plex",
		Item: source.Item{
			ID: "114205", Title: "Wolferton Splash", Kind: source.ItemEpisode,
			ShowTitle: "The Crown", GrandparentID: "114203",
			ParentTitle: "Season 1", ParentID: "114204",
			LibraryID: "2", LibraryTitle: "TV Shows",
		},
	}
	var sb strings.Builder
	if err := breadcrumb(vm).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	for _, want := range []string{"id=114203", "id=114204", "The Crown", "Season 1", `aria-current="page"`} {
		if !strings.Contains(html, want) {
			t.Errorf("breadcrumb HTML missing %q:\n%s", want, html)
		}
	}
	// The current item is text, not a link.
	if strings.Contains(html, `id=114205`) {
		t.Errorf("current item should not be a link in the trail:\n%s", html)
	}
}

func TestChildrenPanelSeasonShowsQueueButton(t *testing.T) {
	vm := ItemDetailVM{
		Source:       "plex",
		Downloadable: true,
		Item:         source.Item{ID: "s1", Title: "Season 1", Kind: source.ItemSeason},
		Children: []source.Item{
			{ID: "e1", Title: "Pilot", Kind: source.ItemEpisode, Container: "mkv", SizeBytes: 1000},
		},
	}
	html := renderComponent(t, ChildrenPanel(vm))
	if !strings.Contains(html, "Queue season") {
		t.Errorf("season panel missing 'Queue season' button:\n%s", html)
	}
	if !strings.Contains(html, "/queue/container?") {
		t.Errorf("season panel missing bulk POST url:\n%s", html)
	}
}

func TestChildrenPanelShowUsesConfirmFlow(t *testing.T) {
	vm := ItemDetailVM{
		Source:       "plex",
		Downloadable: true,
		Item:         source.Item{ID: "sh", Title: "Show", Kind: source.ItemShow},
		Children: []source.Item{
			{ID: "s1", Title: "Season 1", Kind: source.ItemSeason},
		},
	}
	html := renderComponent(t, ChildrenPanel(vm))
	if !strings.Contains(html, "Queue show") {
		t.Errorf("show panel missing 'Queue show' button:\n%s", html)
	}
	if !strings.Contains(html, "/queue/container/confirm?") {
		t.Errorf("show panel must use the confirm flow (GET confirm), got:\n%s", html)
	}
}

func TestChildrenPanelNotDownloadableHasNoBulkButton(t *testing.T) {
	vm := ItemDetailVM{
		Source:       "jellyfin",
		Downloadable: false,
		Item:         source.Item{ID: "s1", Title: "Season 1", Kind: source.ItemSeason},
	}
	html := renderComponent(t, ChildrenPanel(vm))
	if strings.Contains(html, "Queue season") || strings.Contains(html, "Queue show") {
		t.Errorf("non-downloadable source should not offer a bulk queue button:\n%s", html)
	}
}

func TestBulkBannerRendersCounts(t *testing.T) {
	vm := ItemDetailVM{
		Source: "plex", Downloadable: true,
		Item: source.Item{ID: "s1", Title: "Season 1", Kind: source.ItemSeason},
		Bulk: &service.BulkQueueResult{Queued: 3, Skipped: 1, Failed: 0, TotalBytes: 3000},
	}
	html := renderComponent(t, ChildrenPanel(vm))
	if !strings.Contains(html, "Queued <b>3</b>") {
		t.Errorf("banner missing queued count:\n%s", html)
	}
	if !strings.Contains(html, "already present") {
		t.Errorf("banner missing skipped note:\n%s", html)
	}
}

func TestQueueConfirmRendersCountAndSize(t *testing.T) {
	p := service.ContainerPreview{
		Container: "Show", Source: "plex", ItemID: "sh",
		ToQueue: 10, AlreadyHave: 2, Seasons: 3, TotalBytes: 50 << 30,
	}
	html := renderComponent(t, QueueConfirm(p))
	for _, want := range []string{"<b>10</b>", "<b>3</b>", "50.0 GB", "2 already mirrored", "Queue everything"} {
		if !strings.Contains(html, want) {
			t.Errorf("confirm missing %q:\n%s", want, html)
		}
	}
}

func TestQueueConfirmNothingToQueue(t *testing.T) {
	p := service.ContainerPreview{Container: "Show", Source: "plex", ItemID: "sh", ToQueue: 0, AlreadyHave: 5}
	html := renderComponent(t, QueueConfirm(p))
	if !strings.Contains(html, "Nothing to queue") {
		t.Errorf("expected 'Nothing to queue' message:\n%s", html)
	}
	if strings.Contains(html, "Queue everything") {
		t.Errorf("should not offer Queue everything when nothing to queue:\n%s", html)
	}
}

func TestChildrenPanelRendersEvictControls(t *testing.T) {
	season := ItemDetailVM{
		Source: "plex", Downloadable: true,
		Item: source.Item{ID: "s1", Title: "Season 1", Kind: source.ItemSeason},
	}
	if html := renderComponent(t, ChildrenPanel(season)); !strings.Contains(html, "Evict season") {
		t.Errorf("season panel missing Evict season control:\n%s", html)
	}
	show := ItemDetailVM{
		Source: "plex", Downloadable: true,
		Item: source.Item{ID: "sh", Title: "Show", Kind: source.ItemShow},
	}
	if html := renderComponent(t, ChildrenPanel(show)); !strings.Contains(html, "Evict show") {
		t.Errorf("show panel missing Evict show control:\n%s", html)
	}
}

func TestChildrenPanelNotDownloadableHasNoEvictButton(t *testing.T) {
	vm := ItemDetailVM{
		Source: "jellyfin", Downloadable: false,
		Item: source.Item{ID: "s1", Title: "Season 1", Kind: source.ItemSeason},
	}
	html := renderComponent(t, ChildrenPanel(vm))
	if strings.Contains(html, "Evict season") || strings.Contains(html, "Evict show") {
		t.Errorf("non-downloadable source should not offer a bulk evict button:\n%s", html)
	}
}

func TestEvictBannerRendersCounts(t *testing.T) {
	vm := ItemDetailVM{
		Source: "plex", Downloadable: true,
		Item:      source.Item{ID: "s1", Title: "Season 1", Kind: source.ItemSeason},
		EvictBulk: &service.BulkEvictResult{Evicted: 2, Skipped: 1, FreedBytes: 2048},
	}
	html := renderComponent(t, ChildrenPanel(vm))
	if !strings.Contains(html, "Evicted <b>2</b>") {
		t.Errorf("banner missing evicted count:\n%s", html)
	}
	if !strings.Contains(html, "not mirrored") {
		t.Errorf("banner missing skipped note:\n%s", html)
	}
}

func TestEvictConfirmRendersCountAndSize(t *testing.T) {
	p := service.EvictPreview{
		Container: "Show", Source: "plex", ItemID: "sh", Kind: "show",
		ToEvict: 10, Seasons: 3, FreedBytes: 50 << 30,
	}
	html := renderComponent(t, EvictConfirm(p))
	for _, want := range []string{"<b>10</b>", "<b>3</b>", "50.0 GB", "Evict everything"} {
		if !strings.Contains(html, want) {
			t.Errorf("confirm missing %q:\n%s", want, html)
		}
	}
}

func TestEvictConfirmNothingToEvict(t *testing.T) {
	p := service.EvictPreview{Container: "Show", Source: "plex", ItemID: "sh", Kind: "show", ToEvict: 0}
	html := renderComponent(t, EvictConfirm(p))
	if !strings.Contains(html, "Nothing to evict") {
		t.Errorf("expected 'Nothing to evict' message:\n%s", html)
	}
	if strings.Contains(html, "Evict everything") {
		t.Errorf("should not offer Evict everything when nothing to evict:\n%s", html)
	}
}
