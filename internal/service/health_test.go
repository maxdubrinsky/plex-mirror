package service

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/source/plex"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

func openStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return store
}

// healthService builds a Service around a single configurable stub source so the
// monitor/registry logic can be exercised without real network.
func healthService(t *testing.T, name string, srcErr error) (*Service, *browseSource) {
	t.Helper()
	store := openStore(t)
	root := t.TempDir()
	cfg := &config.Config{MediaRoot: root, HealthCheckEvery: 50 * time.Millisecond}
	stub := &browseSource{name: name, libs: []source.Library{{ID: "1", Title: "L"}}, err: srcErr}
	rt := &runtime{
		cfg:     cfg,
		storage: storage.NewManager(store, storage.Policy{MediaRoot: root}),
		sources: map[string]source.Source{name: stub},
	}
	return newWithRuntime(store, rt), stub
}

func netErr() error  { return fmt.Errorf("boom: %w", source.ErrNetwork) }
func authErr() error { return fmt.Errorf("401: %w", source.ErrAuth) }

func TestClassifyHealthErr(t *testing.T) {
	cases := []struct {
		err  error
		want SourceStatus
	}{
		{nil, statusOK},
		{authErr(), statusAuthError},
		{fmt.Errorf("x: %w", source.ErrNotFound), statusParked},
		{netErr(), statusDown},
		{context.DeadlineExceeded, statusDown},
		{errors.New("unclassified"), statusDown},
	}
	for _, c := range cases {
		if got := classifyHealthErr(c.err); got != c.want {
			t.Errorf("classifyHealthErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestHealthBackoffBoundedAndGrows(t *testing.T) {
	s := &Service{healthRand: rand.New(rand.NewSource(1))}
	// attempt 0 sits within ±25% of the base.
	d0 := s.healthBackoff(0)
	if d0 < healthBackoffBase*3/4 || d0 > healthBackoffBase*5/4 {
		t.Errorf("backoff(0) = %v, want within ±25%% of base %v", d0, healthBackoffBase)
	}
	// Large attempt is capped near the max (jitter aside) and never overflows negative.
	big := s.healthBackoff(60)
	if big <= 0 || big > healthBackoffMax*5/4 || big < healthBackoffMax*3/4 {
		t.Errorf("backoff(60) = %v, want near cap %v with no overflow", big, healthBackoffMax)
	}
}

func TestSeedHealthStates(t *testing.T) {
	store := openStore(t)
	svc := newWithRuntime(store, &runtime{cfg: &config.Config{}, sources: map[string]source.Source{}})

	// Plex (explicit URL) + Jellyfin both configured and built → ok, URL recorded.
	eff := &config.Config{PlexURL: "http://pms", PlexToken: "t", JellyfinURL: "http://jf", JellyfinToken: "jt"}
	built := &runtime{cfg: eff, sources: map[string]source.Source{
		"plex":     &browseSource{name: "plex"},
		"jellyfin": &browseSource{name: "jellyfin"},
	}}
	svc.seedHealth(eff, built, nil)
	if h, ok := svc.getHealth("plex"); !ok || h.Status != statusOK || h.ChosenURL != "http://pms" {
		t.Fatalf("plex seed = %+v ok=%v, want ok + chosen url", h, ok)
	}
	if h, ok := svc.getHealth("jellyfin"); !ok || h.Status != statusOK || h.ChosenURL != "http://jf" {
		t.Fatalf("jellyfin seed = %+v ok=%v, want ok", h, ok)
	}

	// disc records the chosen URL, relay flag, and candidate probes.
	reachable := true
	disc := &plex.Discovered{BaseURL: "http://relay", Relay: true,
		Candidates: []plex.ConnectionProbe{{URI: "http://relay", Relay: true, Reachable: &reachable}}}
	svc.seedHealth(eff, built, disc)
	if h, _ := svc.getHealth("plex"); h.ChosenURL != "http://relay" || !h.Relay || len(h.Candidates) != 1 {
		t.Fatalf("plex seed w/ disc = %+v, want relay url + 1 candidate", h)
	}

	// Plex configured via discovery but NOT built (discovery failed at boot) → down.
	eff2 := &config.Config{PlexServer: "S", PlexToken: "t"}
	svc.seedHealth(eff2, &runtime{cfg: eff2, sources: map[string]source.Source{}}, nil)
	if h, ok := svc.getHealth("plex"); !ok || h.Status != statusDown {
		t.Fatalf("unbuilt plex seed = %+v ok=%v, want down (visible)", h, ok)
	}
	// Jellyfin creds gone on reseed → entry pruned.
	if _, ok := svc.getHealth("jellyfin"); ok {
		t.Fatalf("jellyfin entry should be pruned when its creds are removed")
	}

	// All creds gone → plex pruned too.
	svc.seedHealth(&config.Config{}, &runtime{cfg: &config.Config{}, sources: map[string]source.Source{}}, nil)
	if _, ok := svc.getHealth("plex"); ok {
		t.Fatalf("plex entry should be pruned when its creds are removed")
	}
}

func TestProbeAndRecordTransitions(t *testing.T) {
	svc, stub := healthService(t, "plex", nil)
	rt := svc.now()
	ctx := context.Background()

	// Healthy probe → ok, re-check at the normal interval.
	if d := svc.probeAndRecord(ctx, rt, "plex"); d != rt.cfg.HealthCheckEvery {
		t.Errorf("ok wait = %v, want interval %v", d, rt.cfg.HealthCheckEvery)
	}
	if h, _ := svc.getHealth("plex"); h.Status != statusOK {
		t.Errorf("status = %q, want ok", h.Status)
	}

	// Transient network failure → down, positive backoff, reconnect requested.
	stub.err = netErr()
	d := svc.probeAndRecord(ctx, rt, "plex")
	if d <= 0 || d > healthBackoffMax*5/4 {
		t.Errorf("down wait = %v, want a positive backoff", d)
	}
	if h, _ := svc.getHealth("plex"); h.Status != statusDown || h.ConsecutiveFailures == 0 {
		t.Errorf("health = %+v, want down with failures>0", h)
	}
	select {
	case name := <-svc.reconnectCh:
		if name != "plex" {
			t.Errorf("reconnect request = %q, want plex", name)
		}
	default:
		t.Error("transient down must request a reconnect")
	}

	// Auth failure → parked, slow interval, and NO reconnect request (not transient).
	stub.err = authErr()
	if d := svc.probeAndRecord(ctx, rt, "plex"); d != rt.cfg.HealthCheckEvery {
		t.Errorf("parked wait = %v, want slow interval %v", d, rt.cfg.HealthCheckEvery)
	}
	if h, _ := svc.getHealth("plex"); h.Status != statusAuthError {
		t.Errorf("status = %q, want auth_error", h.Status)
	}
	select {
	case <-svc.reconnectCh:
		t.Error("auth error must NOT request a reconnect (no retry storm)")
	default:
	}
}

func TestRecordObservation(t *testing.T) {
	svc, _ := healthService(t, "plex", nil)
	svc.setHealth("plex", func(h *sourceHealth) { h.Status = statusOK })

	// Observed network failure → down + a monitor nudge.
	svc.recordObservation("plex", netErr())
	if h, _ := svc.getHealth("plex"); h.Status != statusDown {
		t.Errorf("status = %q, want down after observed network error", h.Status)
	}
	select {
	case <-svc.kickCh:
	default:
		t.Error("an observed network failure should nudge the monitor")
	}

	// Observed success → ok.
	svc.recordObservation("plex", nil)
	if h, _ := svc.getHealth("plex"); h.Status != statusOK {
		t.Errorf("status = %q, want ok after a successful call", h.Status)
	}

	// A cancelled request is not a source failure → status unchanged.
	svc.recordObservation("plex", context.Canceled)
	if h, _ := svc.getHealth("plex"); h.Status != statusOK {
		t.Errorf("status = %q, want ok (context.Canceled ignored)", h.Status)
	}

	// Untracked source → no-op (no panic, no phantom entry).
	svc.recordObservation("ghost", netErr())
	if _, ok := svc.getHealth("ghost"); ok {
		t.Error("recordObservation must not create entries for untracked sources")
	}
}

func TestReconnectSourceSwitchesConnection(t *testing.T) {
	store := openStore(t)
	root := t.TempDir()
	base := &config.Config{MediaRoot: root, PlexServer: "S", PlexToken: "tok", DownloadConcurrency: 2}
	svc := newWithRuntime(store, &runtime{
		cfg:     base,
		storage: storage.NewManager(store, storage.Policy{MediaRoot: root}),
		sources: map[string]source.Source{}, // plex unbuilt → down
	})
	svc.seedHealth(base, svc.now(), nil)
	if h, _ := svc.getHealth("plex"); h.Status != statusDown {
		t.Fatalf("precondition: plex should be down, got %q", h.Status)
	}

	reachable := true
	defer stubDiscover(func(context.Context, plex.DiscoverOptions) (plex.Discovered, error) {
		return plex.Discovered{
			Name: "S", BaseURL: "http://pms.test", AccessToken: "rtok",
			Candidates: []plex.ConnectionProbe{{URI: "http://pms.test", Reachable: &reachable}},
		}, nil
	})()

	view, err := svc.ReconnectSource(context.Background(), "plex")
	if err != nil {
		t.Fatalf("ReconnectSource: %v", err)
	}
	if view.Status != "ok" || view.ChosenURL != "http://pms.test" {
		t.Fatalf("after reconnect view = %+v, want ok + chosen url", view)
	}
	if _, ok := svc.now().sources["plex"]; !ok {
		t.Error("plex source should be built after a successful reconnect")
	}
	if svc.now().engine == nil {
		t.Error("download engine should be restored alongside the source")
	}
}

func TestReconnectNoReachableConnectionDoesNotSwap(t *testing.T) {
	store := openStore(t)
	root := t.TempDir()
	base := &config.Config{MediaRoot: root, PlexServer: "S", PlexToken: "tok", DownloadConcurrency: 2}
	svc := newWithRuntime(store, &runtime{
		cfg:     base,
		storage: storage.NewManager(store, storage.Policy{MediaRoot: root}),
		sources: map[string]source.Source{},
	})
	svc.seedHealth(base, svc.now(), nil)

	unreachable := false
	defer stubDiscover(func(context.Context, plex.DiscoverOptions) (plex.Discovered, error) {
		// plex.Discover falls back to a top-ranked connection even when nothing is
		// reachable — the reconnect must NOT install it.
		return plex.Discovered{
			Name: "S", BaseURL: "http://dead", AccessToken: "x",
			Candidates: []plex.ConnectionProbe{{URI: "http://dead", Reachable: &unreachable}},
		}, nil
	})()

	view, err := svc.ReconnectSource(context.Background(), "plex")
	if err == nil {
		t.Fatal("want an error when no candidate connection is reachable")
	}
	if view.Status != "down" {
		t.Errorf("status = %q, want down", view.Status)
	}
	if _, ok := svc.now().sources["plex"]; ok {
		t.Error("plex must NOT be built from an unreachable connection")
	}
	if len(view.Candidates) != 1 {
		t.Errorf("candidates = %d, want the probe table recorded for diagnostics", len(view.Candidates))
	}
}

// TestStartReloadNoDeadlockWithHealthMonitor is the regression guard for the
// supervisor design: a reconnect initiated by the in-waitgroup health monitor must
// not deadlock against reloadWith's stopWorkersLocked → wg.Wait(). The stubbed
// PMS fails the first library probe (so the monitor goes down and asks the
// supervisor to reconnect) then succeeds, so a correct implementation recovers to
// ok; a deadlocking one never re-probes and times out.
func TestStartReloadNoDeadlockWithHealthMonitor(t *testing.T) {
	var calls atomic.Int64
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError) // first probe fails
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[]}}`))
			return
		}
		w.WriteHeader(http.StatusOK) // /identity etc.
	}))
	defer pms.Close()

	reachable := true
	defer stubDiscover(func(context.Context, plex.DiscoverOptions) (plex.Discovered, error) {
		return plex.Discovered{
			Name: "S", BaseURL: pms.URL, AccessToken: "rtok",
			Candidates: []plex.ConnectionProbe{{URI: pms.URL, Reachable: &reachable}},
		}, nil
	})()

	store := openStore(t)
	cfg := &config.Config{
		MediaRoot:           t.TempDir(),
		PlexServer:          "S",
		PlexToken:           "tok",
		DownloadConcurrency: 2,
		HealthCheckEvery:    25 * time.Millisecond,
	}
	svc, err := New(cfg, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	// The monitor must fail once, drive a reconnect through the supervisor, re-probe,
	// and land on ok — all without deadlocking. Recovery within the deadline proves it.
	deadline := time.Now().Add(4 * time.Second)
	for {
		if h, ok := svc.SourceHealthFor("plex"); ok && h.Status == "ok" && calls.Load() >= 2 {
			break // reconnect path executed end-to-end
		}
		if time.Now().After(deadline) {
			h, _ := svc.SourceHealthFor("plex")
			t.Fatalf("plex did not recover via reconnect (status=%q calls=%d) — likely a monitor/Reload deadlock", h.Status, calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stubDiscover swaps the package-level discovery seam for a test, returning a
// restore func to defer.
func stubDiscover(fn func(context.Context, plex.DiscoverOptions) (plex.Discovered, error)) func() {
	orig := plexDiscover
	plexDiscover = fn
	return func() { plexDiscover = orig }
}
