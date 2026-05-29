package plex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

// fixtures use Plex's JSON shape; the upstream library sends Accept:
// application/json and Plex obliges. Captured/condensed from real responses.
const (
	librarySectionsJSON = `{
	  "MediaContainer": {
	    "size": 3,
	    "allowSync": false,
	    "identifier": "com.plexapp.plugins.library",
	    "Directory": [
	      {"key": "1", "type": "movie",  "title": "Movies"},
	      {"key": "2", "type": "show",   "title": "TV Shows"},
	      {"key": "3", "type": "artist", "title": "Music"},
	      {"key": "4", "type": "photo",  "title": "Photos"}
	    ]
	  }
	}`

	librarySectionAllJSON = `{
	  "MediaContainer": {
	    "size": 2,
	    "librarySectionID": 1,
	    "librarySectionTitle": "Movies",
	    "Metadata": [
	      {
	        "ratingKey": "1001",
	        "title": "Solaris",
	        "type": "movie",
	        "year": 1972,
	        "Media": [{
	          "container": "mkv",
	          "Part": [{
	            "id": "11",
	            "key": "/library/parts/11/0/file.mkv",
	            "container": "mkv",
	            "size": 5368709120,
	            "file": "/data/movies/Solaris.mkv"
	          }]
	        }]
	      },
	      {
	        "ratingKey": "1002",
	        "title": "Stalker",
	        "type": "movie",
	        "year": 1979,
	        "Media": [{
	          "container": "mp4",
	          "Part": [{
	            "id": "12",
	            "key": "/library/parts/12/0/file.mp4",
	            "container": "mp4",
	            "size": 4294967296
	          },
	          {
	            "id": "13",
	            "key": "/library/parts/13/0/file.mp4",
	            "container": "mp4",
	            "size": 1073741824
	          }]
	        }]
	      }
	    ]
	  }
	}`

	metadataJSON = `{
	  "MediaContainer": {
	    "size": 1,
	    "Metadata": [{
	      "ratingKey": "1001",
	      "title": "Solaris",
	      "type": "movie",
	      "year": 1972,
	      "Media": [{
	        "container": "mkv",
	        "Part": [{
	          "id": "11",
	          "key": "/library/parts/11/0/file.mkv",
	          "container": "mkv",
	          "size": 5368709120
	        }]
	      }]
	    }]
	  }
	}`

	emptyMetadataJSON = `{"MediaContainer": {"size": 0}}`
)

type routeHandler struct {
	t        *testing.T
	handlers map[string]http.HandlerFunc
	// observed records every request path the test server saw, for asserts.
	observed []*http.Request
}

func newRouter(t *testing.T) *routeHandler {
	t.Helper()
	return &routeHandler{t: t, handlers: map[string]http.HandlerFunc{}}
}

func (r *routeHandler) on(path string, h http.HandlerFunc) {
	r.handlers[path] = h
}

func (r *routeHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.observed = append(r.observed, req)
	if h, ok := r.handlers[req.URL.Path]; ok {
		h(w, req)
		return
	}
	r.t.Logf("unexpected request: %s %s", req.Method, req.URL.String())
	http.NotFound(w, req)
}

// newClient spins up an httptest.Server with the given router and returns a
// *Client pointing at it. The server is auto-closed via t.Cleanup.
func newClient(t *testing.T, r *routeHandler) *Client {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	s, err := New(Config{BaseURL: srv.URL, Token: "test-token", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.(*Client)
}

func jsonOK(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(code), code)
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing base url", Config{Token: "t"}},
		{"missing token", Config{BaseURL: "http://localhost"}},
		{"invalid base url", Config{BaseURL: "not-a-url", Token: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestName(t *testing.T) {
	r := newRouter(t)
	c := newClient(t, r)
	if got := c.Name(); got != "plex" {
		t.Fatalf("Name() = %q, want %q", got, "plex")
	}
}

func TestListLibraries(t *testing.T) {
	r := newRouter(t)
	r.on("/library/sections", func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("X-Plex-Token"); got != "test-token" {
			t.Errorf("X-Plex-Token = %q, want %q", got, "test-token")
		}
		if got := req.Header.Get("Accept"); !strings.Contains(got, "application/json") {
			t.Errorf("Accept = %q, want application/json", got)
		}
		jsonOK(librarySectionsJSON)(w, req)
	})

	c := newClient(t, r)
	libs, err := c.ListLibraries(context.Background())
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}

	want := []source.Library{
		{ID: "1", Title: "Movies", Kind: source.LibraryMovies},
		{ID: "2", Title: "TV Shows", Kind: source.LibraryShows},
		{ID: "3", Title: "Music", Kind: source.LibraryMusic},
		{ID: "4", Title: "Photos", Kind: source.LibraryOther},
	}
	if len(libs) != len(want) {
		t.Fatalf("got %d libs, want %d (%+v)", len(libs), len(want), libs)
	}
	for i, got := range libs {
		if got != want[i] {
			t.Errorf("lib[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestListItems(t *testing.T) {
	r := newRouter(t)
	r.on("/library/sections/1/all", func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("X-Plex-Container-Start"); got != "10" {
			t.Errorf("X-Plex-Container-Start = %q, want 10", got)
		}
		if got := req.Header.Get("X-Plex-Container-Size"); got != "25" {
			t.Errorf("X-Plex-Container-Size = %q, want 25", got)
		}
		jsonOK(librarySectionAllJSON)(w, req)
	})

	c := newClient(t, r)
	items, err := c.ListItems(context.Background(), "1", source.ListOptions{Offset: 10, Limit: 25})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}

	first := items[0]
	if first.ID != "1001" || first.Title != "Solaris" || first.Year != 1972 {
		t.Errorf("items[0] basic fields wrong: %+v", first)
	}
	if first.Kind != source.ItemMovie {
		t.Errorf("items[0].Kind = %q, want movie", first.Kind)
	}
	if first.Container != "mkv" {
		t.Errorf("items[0].Container = %q, want mkv", first.Container)
	}
	if first.SizeBytes != 5368709120 {
		t.Errorf("items[0].SizeBytes = %d, want 5368709120", first.SizeBytes)
	}

	second := items[1]
	// Stalker has two parts — size should be the sum.
	if second.SizeBytes != 4294967296+1073741824 {
		t.Errorf("items[1].SizeBytes = %d, want sum of parts", second.SizeBytes)
	}
	if second.Container != "mp4" {
		t.Errorf("items[1].Container = %q, want mp4", second.Container)
	}
}

// Regression for glb-gdl.9: a Limit with no Offset must still send
// X-Plex-Container-Start=0, because Plex ignores Size unless Start is present
// (verified live — without Start the full section came back).
func TestListItems_LimitWithoutOffsetSendsStartZero(t *testing.T) {
	r := newRouter(t)
	r.on("/library/sections/1/all", func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("X-Plex-Container-Start"); got != "0" {
			t.Errorf("X-Plex-Container-Start = %q, want 0 (must be present alongside Size)", got)
		}
		if got := req.Header.Get("X-Plex-Container-Size"); got != "3" {
			t.Errorf("X-Plex-Container-Size = %q, want 3", got)
		}
		jsonOK(librarySectionAllJSON)(w, req)
	})
	c := newClient(t, r)
	if _, err := c.ListItems(context.Background(), "1", source.ListOptions{Limit: 3}); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
}

func TestListItems_NoOptsOmitsHeaders(t *testing.T) {
	r := newRouter(t)
	r.on("/library/sections/1/all", func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("X-Plex-Container-Start"); got != "" {
			t.Errorf("X-Plex-Container-Start should be unset, got %q", got)
		}
		if got := req.Header.Get("X-Plex-Container-Size"); got != "" {
			t.Errorf("X-Plex-Container-Size should be unset, got %q", got)
		}
		jsonOK(librarySectionAllJSON)(w, req)
	})
	c := newClient(t, r)
	if _, err := c.ListItems(context.Background(), "1", source.ListOptions{}); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
}

// A Query is pushed to Plex as ?title= so search spans the whole section
// server-side rather than filtering the page we'd otherwise fetch.
func TestListItems_QueryAddsTitleParam(t *testing.T) {
	r := newRouter(t)
	r.on("/library/sections/1/all", func(w http.ResponseWriter, req *http.Request) {
		if got := req.URL.Query().Get("title"); got != "the crown" {
			t.Errorf("title param = %q, want %q", got, "the crown")
		}
		jsonOK(librarySectionAllJSON)(w, req)
	})
	c := newClient(t, r)
	if _, err := c.ListItems(context.Background(), "1", source.ListOptions{Query: "the crown"}); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
}

// ListChildren hits /library/metadata/{id}/children and maps episodes (which
// carry Media/Part) straight through metadataToItem, with sNNeNN populated.
func TestListChildren(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/114204/children", jsonOK(`{
		"MediaContainer": {
			"Metadata": [
				{"ratingKey":"114205","title":"Wolferton Splash","type":"episode","grandparentTitle":"The Crown","parentIndex":1,"index":1,
				 "Media":[{"Part":[{"key":"/library/parts/227937/1/file.mkv","size":5647285821,"container":"mkv"}]}]},
				{"ratingKey":"114206","title":"Hyde Park Corner","type":"episode","grandparentTitle":"The Crown","parentIndex":1,"index":2,
				 "Media":[{"Part":[{"key":"/library/parts/227938/1/file.mkv","size":4000000000,"container":"mkv"}]}]}
			]
		}
	}`))

	c := newClient(t, r)
	eps, err := c.ListChildren(context.Background(), "114204", source.ListOptions{})
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("got %d children, want 2", len(eps))
	}
	e := eps[0]
	if e.ID != "114205" || e.Kind != source.ItemEpisode {
		t.Errorf("child[0] = %+v, want episode 114205", e)
	}
	if e.ShowTitle != "The Crown" || e.SeasonNumber != 1 || e.EpisodeNumber != 1 {
		t.Errorf("child[0] sNNeNN wrong: show=%q s=%d e=%d", e.ShowTitle, e.SeasonNumber, e.EpisodeNumber)
	}
	if e.Container != "mkv" || e.SizeBytes != 5647285821 {
		t.Errorf("child[0] part fields wrong: container=%q size=%d", e.Container, e.SizeBytes)
	}
}

func TestListChildren_RequiresID(t *testing.T) {
	c := newClient(t, newRouter(t))
	if _, err := c.ListChildren(context.Background(), "", source.ListOptions{}); err == nil {
		t.Fatal("expected error for empty itemID")
	}
}

func TestGetMetadata(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/1001", jsonOK(metadataJSON))

	c := newClient(t, r)
	item, err := c.GetMetadata(context.Background(), "1001")
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if item.ID != "1001" || item.Title != "Solaris" || item.Kind != source.ItemMovie {
		t.Errorf("metadata = %+v", item)
	}
	if item.SizeBytes != 5368709120 {
		t.Errorf("SizeBytes = %d", item.SizeBytes)
	}
}

// The /library/metadata/<id> detail endpoint returns `Rating` as an ARRAY of
// rating tags (plus other array-valued tags), which is the exact shape that
// crashed a decode into gplex.Metadata (scalar Rating float64). This fixture
// is condensed from a real Plex 1.43 detail response; the test guards against
// regressing back onto a struct that can't tolerate it.
const metadataDetailWithRatingArrayJSON = `{
  "MediaContainer": {
    "size": 1,
    "Metadata": [{
      "ratingKey": "100045",
      "title": "Bo Burnham: what.",
      "type": "movie",
      "year": 2013,
      "rating": [
        {"image": "imdb://image.rating", "type": "audience", "value": 8.5},
        {"image": "rottentomatoes://image.rating.ripe", "type": "critic", "value": 9.0}
      ],
      "audienceRating": 8.7,
      "Guid": [{"id": "imdb://tt3139090"}, {"id": "tmdb://244001"}],
      "Media": [{
        "container": "mkv",
        "Part": [{
          "id": "208800",
          "key": "/library/parts/208800/1745037971/file.mkv",
          "container": "mkv",
          "size": 1872673783
        }]
      }]
    }]
  }
}`

func TestGetMetadata_RatingArrayDoesNotBreakDecode(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/100045", jsonOK(metadataDetailWithRatingArrayJSON))

	c := newClient(t, r)
	item, err := c.GetMetadata(context.Background(), "100045")
	if err != nil {
		t.Fatalf("GetMetadata with array-shaped rating: %v", err)
	}
	if item.ID != "100045" || item.Title != "Bo Burnham: what." || item.Year != 2013 {
		t.Errorf("metadata = %+v", item)
	}
	if item.SizeBytes != 1872673783 {
		t.Errorf("SizeBytes = %d, want 1872673783", item.SizeBytes)
	}

	// Same response shape must resolve a download URL without choking.
	target, err := c.ResolveDownloadURL(context.Background(), "100045")
	if err != nil {
		t.Fatalf("ResolveDownloadURL with array-shaped rating: %v", err)
	}
	if !strings.HasSuffix(target.URL, "/library/parts/208800/1745037971/file.mkv?download=1") {
		t.Errorf("unexpected download URL: %q", target.URL)
	}
}

// Episodes/seasons must carry the parent/grandparent + library links so the
// portal can build breadcrumbs up the hierarchy (glb-gdl.10).
func TestGetMetadataPopulatesHierarchyLinks(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/114205", jsonOK(`{
		"MediaContainer": {"Metadata": [{
			"ratingKey":"114205","title":"Wolferton Splash","type":"episode","year":2016,
			"grandparentTitle":"The Crown","grandparentRatingKey":"114203",
			"parentTitle":"Season 1","parentRatingKey":"114204",
			"parentIndex":1,"index":1,
			"librarySectionID":2,"librarySectionTitle":"TV Shows",
			"Media":[{"Part":[{"key":"/library/parts/227937/1/file.mkv","size":5647285821,"container":"mkv"}]}]
		}]}
	}`))
	r.on("/library/metadata/114204", jsonOK(`{
		"MediaContainer": {"Metadata": [{
			"ratingKey":"114204","title":"Season 1","type":"season",
			"parentTitle":"The Crown","parentRatingKey":"114203","index":1,
			"librarySectionID":2,"librarySectionTitle":"TV Shows"
		}]}
	}`))
	c := newClient(t, r)

	ep, err := c.GetMetadata(context.Background(), "114205")
	if err != nil {
		t.Fatalf("GetMetadata episode: %v", err)
	}
	if ep.GrandparentID != "114203" || ep.ShowTitle != "The Crown" {
		t.Errorf("episode grandparent = (%q,%q), want (114203, The Crown)", ep.GrandparentID, ep.ShowTitle)
	}
	if ep.ParentID != "114204" || ep.ParentTitle != "Season 1" {
		t.Errorf("episode parent = (%q,%q), want (114204, Season 1)", ep.ParentID, ep.ParentTitle)
	}
	if ep.LibraryID != "2" || ep.LibraryTitle != "TV Shows" {
		t.Errorf("episode library = (%q,%q), want (2, TV Shows)", ep.LibraryID, ep.LibraryTitle)
	}

	season, err := c.GetMetadata(context.Background(), "114204")
	if err != nil {
		t.Fatalf("GetMetadata season: %v", err)
	}
	if season.Kind != source.ItemSeason {
		t.Errorf("season Kind = %q, want season", season.Kind)
	}
	if season.ParentID != "114203" || season.ParentTitle != "The Crown" {
		t.Errorf("season parent = (%q,%q), want (114203, The Crown)", season.ParentID, season.ParentTitle)
	}
	if season.GrandparentID != "" {
		t.Errorf("season GrandparentID = %q, want empty", season.GrandparentID)
	}
	if season.LibraryID != "2" {
		t.Errorf("season LibraryID = %q, want 2", season.LibraryID)
	}
}

// A movie carries no hierarchy links — breadcrumbs fall back to the library root.
func TestGetMetadataMovieHasNoParentLinks(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/1001", jsonOK(metadataJSON))
	c := newClient(t, r)
	m, err := c.GetMetadata(context.Background(), "1001")
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if m.ParentID != "" || m.GrandparentID != "" {
		t.Errorf("movie parent/grandparent = (%q,%q), want empty", m.ParentID, m.GrandparentID)
	}
}

func TestGetMetadata_EmptyContainerIsNotFound(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/9999", jsonOK(emptyMetadataJSON))

	c := newClient(t, r)
	_, err := c.GetMetadata(context.Background(), "9999")
	if !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestResolveDownloadURL(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/1001", jsonOK(metadataJSON))

	c := newClient(t, r)
	target, err := c.ResolveDownloadURL(context.Background(), "1001")
	if err != nil {
		t.Fatalf("ResolveDownloadURL: %v", err)
	}

	wantURL := c.baseURL + "/library/parts/11/0/file.mkv?download=1"
	if target.URL != wantURL {
		t.Errorf("URL = %q, want %q", target.URL, wantURL)
	}
	if got := target.Headers.Get("X-Plex-Token"); got != "test-token" {
		t.Errorf("X-Plex-Token header = %q, want test-token", got)
	}
	// Token must NOT appear in the URL — we put it in the header.
	if strings.Contains(target.URL, "X-Plex-Token") {
		t.Errorf("URL contains X-Plex-Token, should not: %q", target.URL)
	}
}

func TestResolveDownloadURL_NoParts(t *testing.T) {
	const noPartsJSON = `{
	  "MediaContainer": {
	    "size": 1,
	    "Metadata": [{
	      "ratingKey": "2001",
	      "title": "Broken",
	      "type": "movie",
	      "Media": []
	    }]
	  }
	}`

	r := newRouter(t)
	r.on("/library/metadata/2001", jsonOK(noPartsJSON))

	c := newClient(t, r)
	_, err := c.ResolveDownloadURL(context.Background(), "2001")
	if !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for item with no parts, got %v", err)
	}
}

func TestAuthErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"401", http.StatusUnauthorized},
		{"403", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(t)
			r.on("/library/sections", status(tc.code))

			c := newClient(t, r)
			_, err := c.ListLibraries(context.Background())
			if !errors.Is(err, source.ErrAuth) {
				t.Fatalf("expected ErrAuth for HTTP %d, got %v", tc.code, err)
			}
		})
	}
}

func TestNotFoundMapping(t *testing.T) {
	r := newRouter(t)
	r.on("/library/metadata/missing", status(http.StatusNotFound))

	c := newClient(t, r)
	_, err := c.GetMetadata(context.Background(), "missing")
	if !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Same mapping path for ResolveDownloadURL.
	_, err = c.ResolveDownloadURL(context.Background(), "missing")
	if !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on resolve, got %v", err)
	}
}

func TestNetworkErrorMapping(t *testing.T) {
	// Point at an address that should fail to connect — port 1 is reserved
	// and almost never has a listener.
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", Token: "x", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, gotErr := c.ListLibraries(context.Background())
	if !errors.Is(gotErr, source.ErrNetwork) {
		t.Fatalf("expected ErrNetwork, got %v", gotErr)
	}
}

func TestContextCancellation(t *testing.T) {
	r := newRouter(t)
	r.on("/library/sections", jsonOK(librarySectionsJSON))
	c := newClient(t, r)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.ListLibraries(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestDownloadResolverInterface(t *testing.T) {
	// Sanity: the concrete type satisfies source.DownloadResolver so callers
	// can type-assert the value returned from New.
	r := newRouter(t)
	c := newClient(t, r)
	var _ source.DownloadResolver = c
}
