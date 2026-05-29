package jellyfin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

const testUserID = "user-1"
const testToken = "test-token"

// newTestServer wires an httptest.Server to a routing function and returns a
// configured source ready for the unit under test. Each test owns its own
// server so per-endpoint behavior (e.g. forcing a 401) stays isolated.
func newTestServer(t *testing.T, handler http.HandlerFunc) (source.Source, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	src, err := New(Config{
		BaseURL:    srv.URL,
		Token:      testToken,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return src, srv
}

// writeJSON sets Content-Type explicitly. Go's http.DetectContentType doesn't
// recognise JSON, so naked w.Write of a JSON body gets sniffed as text/plain
// and the OpenAPI client then refuses it with "undefined response type".
func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// usersMeBody is the canned /Users/Me response shared by happy-path tests.
const usersMeBody = `{"Id":"` + testUserID + `","Name":"tester"}`

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{Token: "x"}); err == nil {
		t.Fatal("expected error when BaseURL is empty")
	}
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Fatal("expected error when Token is empty")
	}
}

func TestNameIsJellyfin(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	if got := src.Name(); got != "jellyfin" {
		t.Fatalf("Name = %q, want jellyfin", got)
	}
}

func TestListLibraries(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Confirm the auth header travels with every call so we catch a
		// regression where the default headers stop being attached.
		if got := r.Header.Get("Authorization"); !strings.Contains(got, `MediaBrowser Token="`+testToken+`"`) {
			t.Errorf("Authorization header = %q", got)
		}
		switch r.URL.Path {
		case "/Users/Me":
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, usersMeBody)
		case "/UserViews":
			if got := r.URL.Query().Get("userId"); got != testUserID {
				t.Errorf("userId = %q, want %q", got, testUserID)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"Items":[
					{"Id":"lib-1","Name":"Movies","CollectionType":"movies"},
					{"Id":"lib-2","Name":"Shows","CollectionType":"tvshows"},
					{"Id":"lib-3","Name":"Music","CollectionType":"music"},
					{"Id":"lib-4","Name":"Home Videos","CollectionType":"homevideos"},
					{"Id":"lib-5","Name":"Unknown"}
				],
				"TotalRecordCount":5
			}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	libs, err := src.ListLibraries(context.Background())
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	wantKinds := map[string]source.LibraryKind{
		"lib-1": source.LibraryMovies,
		"lib-2": source.LibraryShows,
		"lib-3": source.LibraryMusic,
		"lib-4": source.LibraryOther,
		"lib-5": source.LibraryOther,
	}
	if len(libs) != len(wantKinds) {
		t.Fatalf("got %d libs, want %d", len(libs), len(wantKinds))
	}
	for _, l := range libs {
		if wantKinds[l.ID] != l.Kind {
			t.Errorf("lib %s kind = %s, want %s", l.ID, l.Kind, wantKinds[l.ID])
		}
		if l.Title == "" {
			t.Errorf("lib %s missing title", l.ID)
		}
	}
}

func TestListItemsPopulatesPlayed(t *testing.T) {
	var capturedQuery url.Values
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			writeJSON(w, usersMeBody)
		case "/Items":
			capturedQuery = r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			// Three items: played=true, played=false, no UserData (should
			// stay nil so the LRU policy can tell "unknown" from "unwatched").
			_, _ = w.Write([]byte(`{
				"Items":[
					{"Id":"i-1","Name":"A","Type":"Movie","ProductionYear":2020,"Container":"mkv","UserData":{"Played":true}},
					{"Id":"i-2","Name":"B","Type":"Movie","UserData":{"Played":false}},
					{"Id":"i-3","Name":"C","Type":"Episode"}
				],
				"TotalRecordCount":3
			}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	items, err := src.ListItems(context.Background(), "lib-1", source.ListOptions{Offset: 10, Limit: 25})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}

	// Pagination must be propagated as query params or the LRU walker will
	// silently re-fetch the same page forever.
	if got := capturedQuery.Get("startIndex"); got != "10" {
		t.Errorf("startIndex = %q, want 10", got)
	}
	if got := capturedQuery.Get("limit"); got != "25" {
		t.Errorf("limit = %q, want 25", got)
	}
	if got := capturedQuery.Get("parentId"); got != "lib-1" {
		t.Errorf("parentId = %q, want lib-1", got)
	}
	if got := capturedQuery.Get("enableUserData"); got != "true" {
		t.Errorf("enableUserData = %q, want true", got)
	}

	// i-1: played
	if items[0].Played == nil || *items[0].Played != true {
		t.Errorf("item[0].Played = %v, want *true", items[0].Played)
	}
	if items[0].Year != 2020 {
		t.Errorf("item[0].Year = %d, want 2020", items[0].Year)
	}
	if items[0].Container != "mkv" {
		t.Errorf("item[0].Container = %q", items[0].Container)
	}
	if items[0].Kind != source.ItemMovie {
		t.Errorf("item[0].Kind = %s, want movie", items[0].Kind)
	}

	// i-2: explicitly unplayed
	if items[1].Played == nil || *items[1].Played != false {
		t.Errorf("item[1].Played = %v, want *false", items[1].Played)
	}

	// i-3: UserData absent → Played must remain nil
	if items[2].Played != nil {
		t.Errorf("item[2].Played = %v, want nil", items[2].Played)
	}
	if items[2].Kind != source.ItemEpisode {
		t.Errorf("item[2].Kind = %s, want episode", items[2].Kind)
	}
}

func TestListItemsPushesSearchTerm(t *testing.T) {
	var gotSearch string
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			writeJSON(w, usersMeBody)
		case "/Items":
			gotSearch = r.URL.Query().Get("searchTerm")
			writeJSON(w, `{"Items":[],"TotalRecordCount":0}`)
		default:
			http.NotFound(w, r)
		}
	})

	if _, err := src.ListItems(context.Background(), "lib-1", source.ListOptions{Query: "the crown"}); err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if gotSearch != "the crown" {
		t.Errorf("searchTerm = %q, want %q", gotSearch, "the crown")
	}
}

func TestListChildrenUsesParentIdNonRecursive(t *testing.T) {
	var q url.Values
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			writeJSON(w, usersMeBody)
		case "/Items":
			q = r.URL.Query()
			writeJSON(w, `{"Items":[
				{"Id":"ep-1","Name":"Wolferton Splash","Type":"Episode","UserData":{"Played":false}},
				{"Id":"ep-2","Name":"Hyde Park Corner","Type":"Episode","UserData":{"Played":true}}
			],"TotalRecordCount":2}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	cl, ok := src.(source.ChildLister)
	if !ok {
		t.Fatal("jellyfin source should implement source.ChildLister")
	}
	items, err := cl.ListChildren(context.Background(), "season-1", source.ListOptions{})
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(items) != 2 || items[0].Kind != source.ItemEpisode {
		t.Fatalf("children = %+v, want 2 episodes", items)
	}
	if got := q.Get("parentId"); got != "season-1" {
		t.Errorf("parentId = %q, want season-1", got)
	}
	if got := q.Get("recursive"); got != "false" {
		t.Errorf("recursive = %q, want false (children must be the immediate level only)", got)
	}
}

func TestGetMetadata(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			writeJSON(w, usersMeBody)
		case "/Items/abc":
			if got := r.URL.Query().Get("userId"); got != testUserID {
				t.Errorf("userId = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"abc","Name":"The Movie","Type":"Movie","ProductionYear":1999,"UserData":{"Played":true}}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	item, err := src.GetMetadata(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if item.ID != "abc" || item.Title != "The Movie" || item.Year != 1999 {
		t.Errorf("unexpected item: %+v", item)
	}
	if item.Kind != source.ItemMovie {
		t.Errorf("Kind = %s, want movie", item.Kind)
	}
	if item.Played == nil || *item.Played != true {
		t.Errorf("Played = %v, want *true", item.Played)
	}
}

// Episodes/seasons must carry parent/grandparent links + sNNeNN so the portal
// breadcrumb (glb-gdl.10) can walk back up the hierarchy.
func TestGetMetadataPopulatesHierarchyLinks(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			writeJSON(w, usersMeBody)
		case "/Items/ep-1":
			writeJSON(w, `{"Id":"ep-1","Name":"Wolferton Splash","Type":"Episode","ProductionYear":2016,
				"SeriesId":"series-1","SeriesName":"The Crown","SeasonId":"season-1","SeasonName":"Season 1",
				"ParentId":"season-1","IndexNumber":1,"ParentIndexNumber":1,"UserData":{"Played":false}}`)
		case "/Items/season-1":
			writeJSON(w, `{"Id":"season-1","Name":"Season 1","Type":"Season",
				"SeriesId":"series-1","SeriesName":"The Crown","ParentId":"series-1","IndexNumber":1}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	ep, err := src.GetMetadata(context.Background(), "ep-1")
	if err != nil {
		t.Fatalf("GetMetadata episode: %v", err)
	}
	if ep.Kind != source.ItemEpisode {
		t.Errorf("ep Kind = %q, want episode", ep.Kind)
	}
	if ep.ShowTitle != "The Crown" || ep.GrandparentID != "series-1" {
		t.Errorf("ep grandparent = (%q,%q), want (The Crown, series-1)", ep.ShowTitle, ep.GrandparentID)
	}
	if ep.ParentID != "season-1" || ep.ParentTitle != "Season 1" {
		t.Errorf("ep parent = (%q,%q), want (season-1, Season 1)", ep.ParentID, ep.ParentTitle)
	}
	if ep.SeasonNumber != 1 || ep.EpisodeNumber != 1 {
		t.Errorf("ep sNNeNN = s%de%d, want s1e1", ep.SeasonNumber, ep.EpisodeNumber)
	}

	season, err := src.GetMetadata(context.Background(), "season-1")
	if err != nil {
		t.Fatalf("GetMetadata season: %v", err)
	}
	if season.Kind != source.ItemSeason {
		t.Errorf("season Kind = %q, want season", season.Kind)
	}
	if season.ParentID != "series-1" || season.ParentTitle != "The Crown" {
		t.Errorf("season parent = (%q,%q), want (series-1, The Crown)", season.ParentID, season.ParentTitle)
	}
	if season.GrandparentID != "" {
		t.Errorf("season GrandparentID = %q, want empty", season.GrandparentID)
	}
}

func TestAuthFailureMapsToErrAuth(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := src.ListLibraries(context.Background())
	if !errors.Is(err, source.ErrAuth) {
		t.Fatalf("err = %v, want wraps ErrAuth", err)
	}
}

func TestNotFoundMapsToErrNotFound(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			writeJSON(w, usersMeBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	_, err := src.GetMetadata(context.Background(), "missing")
	if !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("err = %v, want wraps ErrNotFound", err)
	}
}

func TestForbiddenMapsToErrAuth(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := src.ListLibraries(context.Background())
	if !errors.Is(err, source.ErrAuth) {
		t.Fatalf("err = %v, want wraps ErrAuth", err)
	}
}

// newSourceWithUser builds a source against srv with an explicit User pref so
// tests can exercise the API-key resolution paths.
func newSourceWithUser(t *testing.T, srv *httptest.Server, user string) source.Source {
	t.Helper()
	src, err := New(Config{BaseURL: srv.URL, Token: testToken, User: user, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return src
}

// problemDetails400 mimics Jellyfin's /Users/Me answer to an API key: it is not
// owned by any user, so the endpoint returns 400 rather than 401.
func problemDetails400(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"title":"Token is not owned by a user.","status":400}`))
}

func TestAPIKeyFallsBackToSoleUser(t *testing.T) {
	usersMeCalled, usersListCalled := false, false
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			usersMeCalled = true
			problemDetails400(w)
		case "/Users":
			usersListCalled = true
			writeJSON(w, `[{"Id":"`+testUserID+`","Name":"tester"}]`)
		case "/UserViews":
			if got := r.URL.Query().Get("userId"); got != testUserID {
				t.Errorf("userId = %q, want %q", got, testUserID)
			}
			writeJSON(w, `{"Items":[{"Id":"lib-1","Name":"Movies","CollectionType":"movies"}],"TotalRecordCount":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	libs, err := src.ListLibraries(context.Background())
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	if len(libs) != 1 || libs[0].ID != "lib-1" {
		t.Fatalf("libs = %+v, want one lib-1", libs)
	}
	if !usersMeCalled || !usersListCalled {
		t.Errorf("expected both /Users/Me (%v) and /Users (%v) to be called", usersMeCalled, usersListCalled)
	}
}

func TestAPIKeyMultiUserRequiresConfig(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			problemDetails400(w)
		case "/Users":
			writeJSON(w, `[{"Id":"u-1","Name":"alice"},{"Id":"u-2","Name":"bob"}]`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	_, err := src.ListLibraries(context.Background())
	if err == nil {
		t.Fatal("expected an error when an API key meets a multi-user server")
	}
	if !strings.Contains(err.Error(), "PLEXMIRROR_JELLYFIN_USER") {
		t.Errorf("err = %v, want it to name PLEXMIRROR_JELLYFIN_USER", err)
	}
}

func TestConfiguredUserMatchesByUsernameAndSkipsUsersMe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			t.Errorf("/Users/Me must not be called when a user is configured")
			http.NotFound(w, r)
		case "/Users":
			writeJSON(w, `[{"Id":"u-1","Name":"alice"},{"Id":"u-2","Name":"Bob"}]`)
		case "/UserViews":
			if got := r.URL.Query().Get("userId"); got != "u-2" {
				t.Errorf("userId = %q, want u-2 (matched case-insensitively by name)", got)
			}
			writeJSON(w, `{"Items":[],"TotalRecordCount":0}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// "bob" should match the "Bob" account case-insensitively.
	src := newSourceWithUser(t, srv, "bob")
	if _, err := src.ListLibraries(context.Background()); err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
}

func TestConfiguredUserNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users" {
			writeJSON(w, `[{"Id":"u-1","Name":"alice"}]`)
			return
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	src := newSourceWithUser(t, srv, "nobody")
	_, err := src.ListLibraries(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"nobody" not found`) {
		t.Fatalf("err = %v, want a not-found error naming the configured user", err)
	}
}

// TestErrorDetailSurfacesRawBody guards the diagnostics fix: a non-sentinel HTTP
// error must report the server's JSON body, never the generated client's
// "%!s(*string=...)" formatter noise.
func TestErrorDetailSurfacesRawBody(t *testing.T) {
	src, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			writeJSON(w, usersMeBody)
		case "/Items":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"title":"unknown filter","status":400}`))
		default:
			http.NotFound(w, r)
		}
	})

	_, err := src.ListItems(context.Background(), "lib-1", source.ListOptions{})
	if err == nil {
		t.Fatal("expected an error from a 400 ListItems")
	}
	if strings.Contains(err.Error(), "%!s") {
		t.Errorf("err leaks the client's broken formatter: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown filter") {
		t.Errorf("err = %v, want it to surface the response body's message", err)
	}
}
