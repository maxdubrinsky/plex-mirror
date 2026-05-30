package service

// Source-health tracking + auto/manual reconnect.
//
// Why this exists: Plex is a remote/shared server the operator doesn't control.
// Its connection is resolved once at boot (buildRuntime → plex.Discover) into an
// immutable client; if the remote later restarts or its TLS endpoint goes flaky,
// the client keeps hitting the dead URL with no recovery short of a manual
// Settings save. This file adds a background monitor that probes each source's
// connectivity and, when Plex is unreachable, re-discovers + auto-switches to a
// working connection — plus a manual ReconnectSource for the portal/MCP — and a
// registry that records per-source status for the diagnostics surface.
//
// Locking discipline (four independent, NEVER-nested locks):
//   - s.mu          worker lifecycle + rt swap (in service.go); brief, never held across I/O.
//   - s.healthMu    the registry map only; held for microseconds, read-copy-update.
//   - s.reconnectMu serializes a full reconnect (probe + reloadWith); the OUTER lock —
//                   it may acquire s.mu / s.healthMu (one at a time, each released before
//                   the next), but nothing acquires reconnectMu while holding either.
//   - s.randMu      guards healthRand only.
// s.mu and s.healthMu are never held simultaneously (reloadWith releases s.mu
// before seedHealth takes healthMu). The reconnect supervisor goroutine — the only
// background origin of a Reload — is deliberately outside the worker waitgroup, so
// the health monitor (which IS in the waitgroup) requesting a reconnect can't make
// reloadWith's stopWorkersLocked wait on the goroutine running it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/settings"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/source/plex"
)

// Health-monitor tunables.
const (
	healthProbeTimeout   = 8 * time.Second   // bound on a single liveness ListLibraries probe
	plexReconnectTimeout = 25 * time.Second  // bound on a re-discovery (probes every candidate)
	healthBackoffBase    = 2 * time.Second   // exponential backoff base while a source is down
	healthBackoffMax     = 2 * time.Minute   // backoff ceiling
)

// SourceStatus is a source's connectivity state in the health registry.
type SourceStatus string

const (
	statusUnknown   SourceStatus = "unknown"
	statusOK        SourceStatus = "ok"
	statusDown      SourceStatus = "down"       // transient/network — backoff + (Plex) auto-reconnect
	statusAuthError SourceStatus = "auth_error" // bad token — parked, no fast retry
	statusParked    SourceStatus = "parked"     // non-transient config error (unknown server) — no fast retry
)

// sourceHealth is the internal, mutable per-source record. Stored by VALUE in the
// registry and replaced whole under healthMu (read-copy-update), so no reader ever
// aliases live state. attempt is unexported (backoff bookkeeping, monitor-owned).
type sourceHealth struct {
	Name                string
	Status              SourceStatus
	LastError           string
	LastSuccess         time.Time
	LastCheck           time.Time
	ChosenURL           string
	Relay               bool
	Candidates          []plex.ConnectionProbe
	ConsecutiveFailures int
	NextRetryAt         time.Time
	attempt             int
}

func (h sourceHealth) clone() sourceHealth {
	h.Candidates = cloneCandidates(h.Candidates)
	return h
}

func cloneCandidates(in []plex.ConnectionProbe) []plex.ConnectionProbe {
	if in == nil {
		return nil
	}
	out := make([]plex.ConnectionProbe, len(in))
	for i, c := range in {
		out[i] = c
		if c.Reachable != nil {
			r := *c.Reachable
			out[i].Reachable = &r
		}
	}
	return out
}

// CandidateView is the secret-free per-connection diagnostic for the portal + MCP.
type CandidateView struct {
	URI       string `json:"uri"`
	Protocol  string `json:"protocol"`
	Local     bool   `json:"local"`
	Relay     bool   `json:"relay"`
	Reachable *bool  `json:"reachable"` // tri-state: nil = not probed
}

// SourceHealthView is the secret-free projection of a source's health for the
// portal diagnostics panel and the MCP source_health tool. Times use the
// omitzero tag so an unset time is omitted from JSON rather than rendered as the
// zero date. No credential ever appears here (the URL carries no token).
type SourceHealthView struct {
	Name                string          `json:"name"`
	Status              string          `json:"status"`
	LastError           string          `json:"last_error,omitempty"`
	ChosenURL           string          `json:"chosen_url,omitempty"`
	Relay               bool            `json:"relay"`
	LastSuccess         time.Time       `json:"last_success,omitzero"`
	LastCheck           time.Time       `json:"last_check,omitzero"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	NextRetryAt         time.Time       `json:"next_retry_at,omitzero"`
	Candidates          []CandidateView `json:"candidates,omitempty"`
}

func (h sourceHealth) view() SourceHealthView {
	status := h.Status
	if status == "" {
		status = statusUnknown
	}
	v := SourceHealthView{
		Name:                h.Name,
		Status:              string(status),
		LastError:           h.LastError,
		ChosenURL:           h.ChosenURL,
		Relay:               h.Relay,
		LastSuccess:         h.LastSuccess,
		LastCheck:           h.LastCheck,
		ConsecutiveFailures: h.ConsecutiveFailures,
		NextRetryAt:         h.NextRetryAt,
	}
	for _, c := range h.Candidates {
		v.Candidates = append(v.Candidates, CandidateView{
			URI:       c.URI,
			Protocol:  c.Protocol,
			Local:     c.Local,
			Relay:     c.Relay,
			Reachable: c.Reachable,
		})
	}
	return v
}

// --- registry helpers (all brief under healthMu) --------------------------

func (s *Service) setHealth(name string, mutate func(*sourceHealth)) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.health == nil {
		s.health = map[string]sourceHealth{}
	}
	h := s.health[name]
	h.Name = name
	mutate(&h)
	s.health[name] = h
}

func (s *Service) getHealth(name string) (sourceHealth, bool) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h, ok := s.health[name]
	if !ok {
		return sourceHealth{}, false
	}
	return h.clone(), true
}

func (s *Service) seededNames() []string {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	names := make([]string, 0, len(s.health))
	for n := range s.health {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SourceHealth returns the current health of every tracked source, sorted by
// name. Deep-copied — callers never alias registry state.
func (s *Service) SourceHealth() []SourceHealthView {
	s.healthMu.Lock()
	views := make([]SourceHealthView, 0, len(s.health))
	for _, h := range s.health {
		views = append(views, h.clone().view())
	}
	s.healthMu.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

func (s *Service) getHealthView(name string) (SourceHealthView, bool) {
	h, ok := s.getHealth(name)
	if !ok {
		return SourceHealthView{}, false
	}
	return h.view(), true
}

// SourceHealthFor returns one source's health view, if it's tracked. Used by the
// portal to decide whether to skip a hanging library fetch and show a banner.
func (s *Service) SourceHealthFor(name string) (SourceHealthView, bool) {
	return s.getHealthView(name)
}

func (s *Service) viewOrEmpty(name string) SourceHealthView {
	v, _ := s.getHealthView(name)
	return v
}

// seedHealth (re)seeds the registry from the effective config + the resolved Plex
// connection. Called after every runtime (re)build, it is the single point where
// the set of tracked sources is reconciled: an entry exists iff that source's
// creds are configured (so Plex stays visible as "down" even when its source
// object failed to build), and entries whose creds were removed are dropped. When
// a connection was resolved (disc != nil) it records the chosen URL + candidate
// probes; status is optimistically ok when the source built (the monitor confirms
// within one interval) and down when configured-but-unbuilt.
func (s *Service) seedHealth(eff *config.Config, rt *runtime, disc *plex.Discovered) {
	now := time.Now()

	plexConfigured := eff.PlexToken != "" && (eff.PlexURL != "" || eff.PlexServer != "")
	_, plexBuilt := rt.sources["plex"]
	if plexConfigured {
		s.setHealth("plex", func(h *sourceHealth) {
			if disc != nil {
				h.ChosenURL = disc.BaseURL
				h.Relay = disc.Relay
				h.Candidates = cloneCandidates(disc.Candidates)
			} else if eff.PlexURL != "" {
				h.ChosenURL = eff.PlexURL
				h.Relay = false
				h.Candidates = nil
			}
			h.LastCheck = now
			if plexBuilt {
				h.Status = statusOK
				h.LastError = ""
				h.LastSuccess = now
				h.ConsecutiveFailures = 0
				h.attempt = 0
				h.NextRetryAt = time.Time{}
			} else {
				h.Status = statusDown
				if h.LastError == "" {
					h.LastError = "not connected (Plex discovery failed)"
				}
			}
		})
	} else {
		s.deleteHealth("plex")
	}

	jfConfigured := eff.JellyfinURL != "" && eff.JellyfinToken != ""
	_, jfBuilt := rt.sources["jellyfin"]
	if jfConfigured {
		s.setHealth("jellyfin", func(h *sourceHealth) {
			h.ChosenURL = eff.JellyfinURL
			h.LastCheck = now
			if jfBuilt {
				h.Status = statusOK
				h.LastError = ""
				h.LastSuccess = now
				h.ConsecutiveFailures = 0
			} else {
				h.Status = statusDown
			}
		})
	} else {
		s.deleteHealth("jellyfin")
	}
}

func (s *Service) deleteHealth(name string) {
	s.healthMu.Lock()
	delete(s.health, name)
	s.healthMu.Unlock()
}

// --- classification + backoff ---------------------------------------------

func classifyHealthErr(err error) SourceStatus {
	switch {
	case err == nil:
		return statusOK
	case errors.Is(err, source.ErrAuth):
		return statusAuthError
	case errors.Is(err, source.ErrNotFound):
		return statusParked
	case errors.Is(err, source.ErrNetwork), errors.Is(err, context.DeadlineExceeded):
		return statusDown
	default:
		return statusDown // unknown failures treated as transient
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// healthBackoff returns base*2^attempt capped at healthBackoffMax with ±25%
// jitter (mirrors the download engine's backoff). Guards the shift against
// overflow for a long-running outage.
func (s *Service) healthBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := healthBackoffMax
	if attempt < 31 {
		if shifted := healthBackoffBase << uint(attempt); shifted > 0 && shifted < healthBackoffMax {
			d = shifted
		}
	}
	s.randMu.Lock()
	jitter := time.Duration(float64(d) * (s.healthRand.Float64()*0.5 - 0.25))
	s.randMu.Unlock()
	return d + jitter
}

// --- coalescing channels ---------------------------------------------------

func (s *Service) requestReconnect(name string) {
	select {
	case s.reconnectCh <- name:
	default: // a request is already queued/in flight — coalesce
	}
}

func (s *Service) nudgeMonitor() {
	select {
	case s.kickCh <- struct{}{}:
	default:
	}
}

// --- reconnect supervisor --------------------------------------------------

// startReconnectSupervisorLocked launches the single goroutine that owns all
// background-initiated reconnects/Reloads. Lifetime is ctx (runCtx) — it is NOT
// part of the per-generation worker waitgroup, which is what makes it safe for
// the health monitor to ask for a reconnect without deadlocking on itself.
// Caller must hold s.mu; idempotent.
func (s *Service) startReconnectSupervisorLocked(ctx context.Context) {
	if s.supervisorUp {
		return
	}
	s.supervisorUp = true
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case name := <-s.reconnectCh:
				if _, err := s.doReconnect(ctx, name); err != nil {
					slog.Warn("service: auto-reconnect attempt failed", "source", name, "err", err)
				}
			}
		}
	}()
}

// --- health monitor --------------------------------------------------------

// runHealthMonitor is a per-generation worker (launched in launchWorkersLocked).
// It probes each tracked source's connectivity; on a transient Plex failure it
// asks the supervisor to re-discover + switch connections. The timer is dynamic:
// when everything is ok it waits HealthCheckEvery; when a source is down it waits
// that source's backoff; a kick (a user-observed network failure) probes now.
func (s *Service) runHealthMonitor(ctx context.Context, rt *runtime, every time.Duration) {
	if every <= 0 {
		return // monitor (and thus auto-reconnect) disabled
	}
	timer := time.NewTimer(0) // first probe fires immediately on launch
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kickCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		wait := s.healthTick(ctx, rt)
		if wait <= 0 {
			wait = every
		}
		timer.Reset(wait)
	}
}

// healthTick probes every tracked source once and returns the soonest the monitor
// should wake again (the min of per-source next-due intervals).
func (s *Service) healthTick(ctx context.Context, rt *runtime) time.Duration {
	if ctx.Err() != nil {
		return 0
	}
	wait := rt.cfg.HealthCheckEvery
	for _, name := range s.seededNames() {
		if d := s.probeAndRecord(ctx, rt, name); d > 0 && d < wait {
			wait = d
		}
	}
	return wait
}

// probeAndRecord does one liveness probe of a source against the live runtime,
// updates the registry, and returns when that source is next due. For a transient
// Plex failure it also asks the supervisor to re-discover + reconnect.
func (s *Service) probeAndRecord(ctx context.Context, rt *runtime, name string) time.Duration {
	every := rt.cfg.HealthCheckEvery

	var probeErr error
	if src, built := rt.sources[name]; built {
		pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		_, probeErr = src.ListLibraries(pctx)
		cancel()
	} else {
		// Configured (seeded) but the source object failed to build — e.g. Plex
		// discovery failed at boot. Treat as a transient outage so reconnect retries.
		probeErr = fmt.Errorf("%s: not connected: %w", name, source.ErrNetwork)
	}

	// A cancelled monitor ctx (shutdown / reload teardown) is not a source failure.
	if ctx.Err() != nil {
		return every
	}

	now := time.Now()
	switch classifyHealthErr(probeErr) {
	case statusOK:
		s.setHealth(name, func(h *sourceHealth) {
			h.Status = statusOK
			h.LastError = ""
			h.LastSuccess = now
			h.LastCheck = now
			h.ConsecutiveFailures = 0
			h.attempt = 0
			h.NextRetryAt = time.Time{}
		})
		return every
	case statusAuthError, statusParked:
		st := classifyHealthErr(probeErr)
		s.setHealth(name, func(h *sourceHealth) {
			h.Status = st
			h.LastError = errString(probeErr)
			h.LastCheck = now
			h.ConsecutiveFailures++
			h.attempt = 0
			h.NextRetryAt = now.Add(every)
		})
		return every // parked: slow re-check only, no fast backoff, no auto-switch
	default: // statusDown — transient
		cur, _ := s.getHealth(name)
		backoff := s.healthBackoff(cur.attempt)
		s.setHealth(name, func(h *sourceHealth) {
			h.Status = statusDown
			h.LastError = errString(probeErr)
			h.LastCheck = now
			h.ConsecutiveFailures++
			h.attempt++
			h.NextRetryAt = now.Add(backoff)
		})
		if name == "plex" {
			s.requestReconnect(name) // only Plex can switch connections
		}
		return backoff
	}
}

// --- reconnect -------------------------------------------------------------

// ReconnectSource forces an immediate reconnect attempt for a source (the portal
// "Reconnect now" button + the MCP reconnect_source tool), bypassing the monitor's
// backoff. Returns the refreshed health view.
func (s *Service) ReconnectSource(ctx context.Context, name string) (SourceHealthView, error) {
	if _, ok := s.getHealth(name); !ok {
		return SourceHealthView{}, fmt.Errorf("%w: %q", ErrSourceNotFound, name)
	}
	return s.doReconnect(ctx, name)
}

// doReconnect re-establishes a source's connection. For Plex it re-resolves from
// plex.tv probing ALL candidates and, ONLY if one is reachable, rebuilds the
// runtime with that exact connection (via reloadWith — restoring the source AND
// the download engine atomically). A failed/empty re-discovery records the outcome
// but leaves the existing runtime untouched, so a transient outage never makes
// Plex vanish from the source list. Serialized by reconnectMu.
func (s *Service) doReconnect(ctx context.Context, name string) (SourceHealthView, error) {
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	// Static-URL sources (Jellyfin) can't switch connections — just re-probe.
	if name != "plex" {
		s.probeAndRecord(ctx, s.now(), name)
		return s.viewOrEmpty(name), nil
	}

	eff, err := s.effectiveConfig(ctx)
	if err != nil {
		return s.viewOrEmpty(name), fmt.Errorf("reconnect: load settings: %w", err)
	}
	if eff.PlexToken == "" || (eff.PlexURL == "" && eff.PlexServer == "") {
		return s.viewOrEmpty(name), nil // Plex not configured — nothing to reconnect
	}

	// With an explicit PlexURL there's nothing to discover — rebuild from it and let
	// the monitor confirm. Otherwise re-resolve from plex.tv, probing every candidate
	// so the diagnostics table is complete, and only swap if one is actually reachable.
	var pre *plex.Discovered
	if eff.PlexURL == "" && eff.PlexServer != "" {
		dctx, cancel := context.WithTimeout(ctx, plexReconnectTimeout)
		disc, derr := plexDiscover(dctx, plex.DiscoverOptions{
			Token:            eff.PlexToken,
			Server:           eff.PlexServer,
			ClientIdentifier: eff.PlexClientID,
			ProbeAll:         true,
		})
		cancel()
		if derr != nil {
			st := classifyHealthErr(derr)
			s.setHealth(name, func(h *sourceHealth) {
				h.Status = st
				h.LastError = errString(derr)
				h.LastCheck = time.Now()
			})
			return s.viewOrEmpty(name), derr
		}
		// plex.Discover falls back to the top-ranked connection even when NONE probed
		// reachable. Installing that would mark a dead URL "ok" — so gate the swap on
		// a candidate that actually responded, and record the full probe table either way.
		if !anyReachable(disc.Candidates) {
			s.setHealth(name, func(h *sourceHealth) {
				h.Status = statusDown
				h.LastError = "no reachable Plex connection (all advertised connections failed to respond)"
				h.LastCheck = time.Now()
				h.ConsecutiveFailures++
				h.Candidates = cloneCandidates(disc.Candidates)
			})
			return s.viewOrEmpty(name), errors.New("reconnect: no reachable Plex connection")
		}
		pre = &disc
	}

	if err := s.reloadWith(ctx, pre); err != nil {
		s.setHealth(name, func(h *sourceHealth) {
			h.LastError = errString(err)
			h.LastCheck = time.Now()
		})
		return s.viewOrEmpty(name), err
	}
	// reloadWith re-seeded health (status ok + chosen URL + candidate probes).
	slog.Info("service: reconnected source", "source", name)
	return s.viewOrEmpty(name), nil
}

func anyReachable(cands []plex.ConnectionProbe) bool {
	for _, c := range cands {
		if c.Reachable != nil && *c.Reachable {
			return true
		}
	}
	return false
}

// recordObservation folds a live operation's outcome into the registry so it's
// never staler than the last real request. A success promotes the source to ok;
// an observed network failure flips it to down and nudges the monitor to probe now
// (fast recovery). It never drives the reconnect decision itself (no reentrancy
// into Reload from a request path) — only the monitor/supervisor reconnect.
func (s *Service) recordObservation(name string, err error) {
	if errors.Is(err, context.Canceled) {
		return // the caller (e.g. a portal request) went away — not a source failure
	}
	if _, ok := s.getHealth(name); !ok {
		return // not a tracked source
	}
	now := time.Now()
	st := classifyHealthErr(err)
	switch st {
	case statusOK:
		s.setHealth(name, func(h *sourceHealth) {
			h.Status = statusOK
			h.LastError = ""
			h.LastSuccess = now
			h.LastCheck = now
			h.ConsecutiveFailures = 0
			h.attempt = 0
			h.NextRetryAt = time.Time{}
		})
	case statusAuthError, statusParked:
		s.setHealth(name, func(h *sourceHealth) {
			h.Status = st
			h.LastError = errString(err)
			h.LastCheck = now
		})
	default: // down
		s.setHealth(name, func(h *sourceHealth) {
			h.Status = statusDown
			h.LastError = errString(err)
			h.LastCheck = now
		})
		s.nudgeMonitor()
	}
}

// effectiveConfig recomputes the running effective config (env base overlaid with
// stored settings) for the reconnect path's discovery call.
func (s *Service) effectiveConfig(ctx context.Context) (*config.Config, error) {
	vals, err := s.settings.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	return settings.Effective(s.baseCfg, vals)
}
