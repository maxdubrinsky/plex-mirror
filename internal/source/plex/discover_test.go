package plex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

const acctToken = "account-token-123"

// fakePlexTV serves the /api/v2/resources JSON, asserting the auth + client-id
// headers the real plex.tv requires.
func fakePlexTV(t *testing.T, resourcesJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v2/resources") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Plex-Token") != acctToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Plex-Client-Identifier") == "" {
			http.Error(w, "missing X-Plex-Client-Identifier", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, resourcesJSON)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakePMS serves /identity so connection probes succeed.
func fakePMS(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/identity" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func conn(protocol, uri string, local, relay bool) string {
	return fmt.Sprintf(`{"protocol":%q,"uri":%q,"local":%t,"relay":%t}`, protocol, uri, local, relay)
}

func resourcesJSON(server string, accessToken string, conns ...string) string {
	return fmt.Sprintf(`[{"name":%q,"clientIdentifier":"cid-abc","provides":"server","owned":false,"accessToken":%q,"connections":[%s]}]`,
		server, accessToken, strings.Join(conns, ","))
}

func discover(t *testing.T, plexTVURL, server string) (Discovered, error) {
	t.Helper()
	return Discover(context.Background(), DiscoverOptions{
		Token:        acctToken,
		Server:       server,
		PlexTVURL:    plexTVURL,
		HTTPClient:   &http.Client{Timeout: 3 * time.Second},
		ProbeTimeout: time.Second,
	})
}

func TestDiscoverPrefersRemoteDirectOverRelay(t *testing.T) {
	direct := fakePMS(t)
	relay := fakePMS(t)
	// Relay is listed first to prove ranking — not list order — wins.
	js := resourcesJSON("Friend's Server", "per-resource-tok",
		conn("https", relay.URL, false, true),
		conn("https", direct.URL, false, false),
	)
	tv := fakePlexTV(t, js)

	got, err := discover(t, tv.URL, "friend's server") // case-insensitive
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.BaseURL != direct.URL {
		t.Errorf("BaseURL = %q, want remote-direct %q", got.BaseURL, direct.URL)
	}
	if got.AccessToken != "per-resource-tok" {
		t.Errorf("AccessToken = %q, want per-resource token", got.AccessToken)
	}
	if got.Relay {
		t.Errorf("Relay = true, want false (direct chosen)")
	}
}

func TestDiscoverFallsBackToRelayWhenDirectUnreachable(t *testing.T) {
	relay := fakePMS(t)
	js := resourcesJSON("Server", "tok",
		conn("https", "http://127.0.0.1:1", false, false), // refused → probe fails
		conn("https", relay.URL, false, true),
	)
	tv := fakePlexTV(t, js)

	got, err := discover(t, tv.URL, "Server")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.BaseURL != relay.URL || !got.Relay {
		t.Errorf("got BaseURL=%q relay=%v, want relay %q reachable", got.BaseURL, got.Relay, relay.URL)
	}
}

func TestDiscoverFallsBackToTopRankedWhenNoneReachable(t *testing.T) {
	js := resourcesJSON("Server", "tok",
		conn("https", "http://127.0.0.1:1", false, false), // rank 0, unreachable
		conn("https", "http://127.0.0.1:2", false, true),  // rank 3, unreachable
	)
	tv := fakePlexTV(t, js)

	got, err := discover(t, tv.URL, "Server")
	if err != nil {
		t.Fatalf("Discover (best-effort fallback) erred: %v", err)
	}
	if got.BaseURL != "http://127.0.0.1:1" {
		t.Errorf("BaseURL = %q, want highest-ranked fallback http://127.0.0.1:1", got.BaseURL)
	}
}

func TestDiscoverRecordsCandidatesBreaksOnFirstReachable(t *testing.T) {
	direct := fakePMS(t)
	relay := fakePMS(t)
	js := resourcesJSON("Server", "tok",
		conn("https", direct.URL, false, false), // rank 0, reachable → chosen, loop breaks here
		conn("https", relay.URL, false, true),   // rank 3, never probed by default
	)
	tv := fakePlexTV(t, js)

	got, err := discover(t, tv.URL, "Server")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("Candidates len = %d, want 2", len(got.Candidates))
	}
	if got.Candidates[0].URI != direct.URL || got.Candidates[0].Relay {
		t.Errorf("candidate[0] = %+v, want remote-direct %q", got.Candidates[0], direct.URL)
	}
	if got.Candidates[0].Reachable == nil || !*got.Candidates[0].Reachable {
		t.Errorf("candidate[0].Reachable = %v, want true", got.Candidates[0].Reachable)
	}
	// Break-on-first-reachable leaves the lower-ranked candidate unprobed (tri-state nil).
	if got.Candidates[1].Reachable != nil {
		t.Errorf("candidate[1].Reachable = %v, want nil (not probed)", *got.Candidates[1].Reachable)
	}
}

func TestDiscoverProbeAllMarksEveryCandidate(t *testing.T) {
	direct := fakePMS(t)
	relay := fakePMS(t)
	js := resourcesJSON("Server", "tok",
		conn("https", direct.URL, false, false),           // rank 0, reachable
		conn("https", "http://127.0.0.1:1", false, false), // rank 0, unreachable
		conn("https", relay.URL, false, true),             // rank 3, reachable
	)
	tv := fakePlexTV(t, js)

	got, err := Discover(context.Background(), DiscoverOptions{
		Token:        acctToken,
		Server:       "Server",
		PlexTVURL:    tv.URL,
		HTTPClient:   &http.Client{Timeout: 3 * time.Second},
		ProbeTimeout: time.Second,
		ProbeAll:     true,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Candidates) != 3 {
		t.Fatalf("Candidates len = %d, want 3", len(got.Candidates))
	}
	for i, c := range got.Candidates {
		if c.Reachable == nil {
			t.Errorf("candidate[%d] (%s) Reachable = nil, want every candidate probed with ProbeAll", i, c.URI)
		}
	}
	// The best-ranked *reachable* connection wins — remote-direct, not the relay.
	if got.BaseURL != direct.URL || got.Relay {
		t.Errorf("BaseURL=%q relay=%v, want remote-direct %q", got.BaseURL, got.Relay, direct.URL)
	}
}

func TestDiscoverServerNotFoundListsAvailable(t *testing.T) {
	js := resourcesJSON("Real Server", "tok", conn("https", "http://x", false, false))
	tv := fakePlexTV(t, js)

	_, err := discover(t, tv.URL, "Nonexistent")
	if !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("err = %v, want source.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), `"Real Server"`) {
		t.Errorf("error should list available servers, got: %v", err)
	}
}

func TestDiscoverAuthError(t *testing.T) {
	tv := fakePlexTV(t, "[]")
	_, err := Discover(context.Background(), DiscoverOptions{
		Token:     "wrong-token",
		Server:    "x",
		PlexTVURL: tv.URL,
	})
	if !errors.Is(err, source.ErrAuth) {
		t.Fatalf("err = %v, want source.ErrAuth", err)
	}
}

func TestDiscoverAmbiguousNameRejected(t *testing.T) {
	js := fmt.Sprintf(`[
		{"name":"Home","clientIdentifier":"cid-1","provides":"server","accessToken":"t1","connections":[%s]},
		{"name":"Home","clientIdentifier":"cid-2","provides":"server","accessToken":"t2","connections":[%s]}
	]`, conn("https", "http://a", false, false), conn("https", "http://b", false, false))
	tv := fakePlexTV(t, js)

	// Ambiguous by name.
	if _, err := discover(t, tv.URL, "Home"); err == nil || !strings.Contains(err.Error(), "matches 2 servers") {
		t.Fatalf("ambiguous name err = %v, want 'matches 2 servers'", err)
	}
	// Disambiguated by clientIdentifier.
	got, err := discover(t, tv.URL, "cid-2")
	if err != nil {
		t.Fatalf("by clientIdentifier: %v", err)
	}
	if got.AccessToken != "t2" {
		t.Errorf("AccessToken = %q, want t2 (the cid-2 server)", got.AccessToken)
	}
}

func TestDiscoverIgnoresNonServerResources(t *testing.T) {
	pms := fakePMS(t)
	js := fmt.Sprintf(`[
		{"name":"My Phone","clientIdentifier":"p1","provides":"client,player","accessToken":"x","connections":[]},
		{"name":"Tower","clientIdentifier":"s1","provides":"server","accessToken":"servertok","connections":[%s]}
	]`, conn("https", pms.URL, false, false))
	tv := fakePlexTV(t, js)

	got, err := discover(t, tv.URL, "Tower")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.AccessToken != "servertok" {
		t.Errorf("AccessToken = %q, want servertok", got.AccessToken)
	}
}
