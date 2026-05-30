package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

// DefaultPlexTVURL is the plex.tv host used for account-level server discovery.
const DefaultPlexTVURL = "https://plex.tv"

// DefaultClientIdentifier identifies this app to plex.tv. plex.tv's v2 API
// requires an X-Plex-Client-Identifier; a stable constant is fine for a
// single-tenant homelab service. Override via PLEXMIRROR_PLEX_CLIENT_ID if you
// run several instances against one account and want them distinct in your
// plex.tv device list.
const DefaultClientIdentifier = "plex-mirror"

// DiscoverOptions configures server discovery against plex.tv.
type DiscoverOptions struct {
	// Token is the plex.tv ACCOUNT token (not a per-resource token). Discovery
	// returns the per-resource access token for the matched server.
	Token string
	// Server selects the target by name (case-insensitive) or by exact
	// clientIdentifier — handy when two shares have the same display name.
	Server string
	// ClientIdentifier sets X-Plex-Client-Identifier; defaults to
	// DefaultClientIdentifier.
	ClientIdentifier string
	// HTTPClient is used for both the resources call and connection probes;
	// defaults to a 15s-timeout client. Must verify TLS so a probe that passes
	// also works for real requests (plex.direct certs are valid).
	HTTPClient *http.Client
	// PlexTVURL overrides the plex.tv base (tests point this at a stub).
	PlexTVURL string
	// Probe reports whether a candidate base URL is reachable. Defaults to a
	// GET {base}/identity check. Injectable for tests.
	Probe func(ctx context.Context, baseURL string) bool
	// ProbeTimeout bounds each connection probe; defaults to 4s.
	ProbeTimeout time.Duration
	// ProbeAll, when true, probes every ranked candidate (so Discovered.Candidates
	// reports reachability for all of them) instead of stopping at the first
	// reachable one. The diagnostics / manual-reconnect path sets this for a
	// complete picture; the hot boot path leaves it false to stay fast.
	ProbeAll bool
}

// ConnectionProbe is a per-candidate diagnostic record: a connection plex.tv
// advertised for the server and whether it probed reachable. Reachable is
// tri-state — nil means "not probed" (break-on-first-reachable skipped it), so
// the UI can distinguish "known down" from "untested".
type ConnectionProbe struct {
	URI       string `json:"uri"`
	Protocol  string `json:"protocol"`
	Local     bool   `json:"local"`
	Relay     bool   `json:"relay"`
	Reachable *bool  `json:"reachable"`
}

// Discovered is the resolved connection for a server.
type Discovered struct {
	Name             string
	ClientIdentifier string
	BaseURL          string // chosen connection URI, ready for plex.New
	AccessToken      string // per-resource token to authenticate server calls
	Relay            bool   // true if the chosen connection routes through Plex's (throttled) relay
	// Candidates is every connection the server advertised, best-ranked first,
	// with each one's probe result — for the source-health diagnostics surface.
	Candidates []ConnectionProbe
}

type resourceDTO struct {
	Name             string          `json:"name"`
	Product          string          `json:"product"`
	ClientIdentifier string          `json:"clientIdentifier"`
	Provides         string          `json:"provides"`
	Owned            bool            `json:"owned"`
	AccessToken      string          `json:"accessToken"`
	Connections      []connectionDTO `json:"connections"`
}

type connectionDTO struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	URI      string `json:"uri"`
	Local    bool   `json:"local"`
	Relay    bool   `json:"relay"`
}

// Discover resolves a Plex server's base URL + per-resource access token from
// plex.tv given an account token and a server name. It picks the best reachable
// connection (remote-direct HTTPS first, relay last), so the caller never has
// to hardcode a volatile *.plex.direct URL.
func Discover(ctx context.Context, opts DiscoverOptions) (Discovered, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return Discovered{}, fmt.Errorf("plex discover: account token is required")
	}
	if strings.TrimSpace(opts.Server) == "" {
		return Discovered{}, fmt.Errorf("plex discover: server name is required")
	}
	clientID := opts.ClientIdentifier
	if clientID == "" {
		clientID = DefaultClientIdentifier
	}
	base := opts.PlexTVURL
	if base == "" {
		base = DefaultPlexTVURL
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	probeTimeout := opts.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 4 * time.Second
	}
	probe := opts.Probe
	if probe == nil {
		probe = func(ctx context.Context, baseURL string) bool {
			return probeIdentity(ctx, hc, baseURL)
		}
	}

	resources, err := fetchResources(ctx, hc, base, opts.Token, clientID)
	if err != nil {
		return Discovered{}, err
	}

	res, err := selectServer(resources, opts.Server)
	if err != nil {
		return Discovered{}, err
	}
	if res.AccessToken == "" {
		return Discovered{}, fmt.Errorf("plex discover: server %q has no access token on this account", res.Name)
	}

	conns := rankConnections(res.Connections)
	if len(conns) == 0 {
		return Discovered{}, fmt.Errorf("plex discover: server %q advertised no usable connections", res.Name)
	}

	candidates := make([]ConnectionProbe, len(conns))
	for i, c := range conns {
		candidates[i] = ConnectionProbe{
			URI:      strings.TrimRight(c.URI, "/"),
			Protocol: c.Protocol,
			Local:    c.Local,
			Relay:    c.Relay,
		}
	}

	chosen := conns[0] // best-ranked fallback if nothing probes reachable
	chosenIdx := -1
	for i, c := range conns {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		ok := probe(pctx, c.URI)
		cancel()
		reachable := ok
		candidates[i].Reachable = &reachable
		if ok && chosenIdx == -1 {
			// conns is rank-sorted, so the first reachable is the best reachable.
			chosen, chosenIdx = c, i
			if !opts.ProbeAll {
				break // hot path: stop at the first reachable, leave the rest unprobed (nil)
			}
		}
	}

	return Discovered{
		Name:             res.Name,
		ClientIdentifier: res.ClientIdentifier,
		BaseURL:          strings.TrimRight(chosen.URI, "/"),
		AccessToken:      res.AccessToken,
		Relay:            chosen.Relay,
		Candidates:       candidates,
	}, nil
}

func fetchResources(ctx context.Context, hc *http.Client, plexTVURL, token, clientID string) ([]resourceDTO, error) {
	u := strings.TrimRight(plexTVURL, "/") + "/api/v2/resources?includeHttps=1&includeRelay=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("plex discover: build request: %w", err)
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("X-Plex-Client-Identifier", clientID)
	req.Header.Set("X-Plex-Product", "plex-mirror")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex discover: %w: %v", source.ErrNetwork, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("plex discover: plex.tv rejected the account token: %w", source.ErrAuth)
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("plex discover: plex.tv returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var resources []resourceDTO
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		return nil, fmt.Errorf("plex discover: decode resources: %w", err)
	}
	return resources, nil
}

// selectServer finds the one server resource matching want (by name,
// case-insensitive, or exact clientIdentifier). It returns a helpful error
// listing the available servers when there's no match, and refuses an ambiguous
// name match across multiple servers.
func selectServer(resources []resourceDTO, want string) (resourceDTO, error) {
	var servers []resourceDTO
	for _, r := range resources {
		if isServer(r) {
			servers = append(servers, r)
		}
	}

	var matches []resourceDTO
	for _, r := range servers {
		if strings.EqualFold(r.Name, want) || r.ClientIdentifier == want {
			matches = append(matches, r)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return resourceDTO{}, fmt.Errorf("plex discover: no server named %q on this account (available: %s): %w",
			want, availableNames(servers), source.ErrNotFound)
	default:
		return resourceDTO{}, fmt.Errorf("plex discover: %q matches %d servers; set PLEXMIRROR_PLEX_SERVER to a clientIdentifier to disambiguate",
			want, len(matches))
	}
}

func isServer(r resourceDTO) bool {
	for _, p := range strings.Split(r.Provides, ",") {
		if strings.TrimSpace(p) == "server" {
			return true
		}
	}
	return false
}

func availableNames(servers []resourceDTO) string {
	if len(servers) == 0 {
		return "none"
	}
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, fmt.Sprintf("%q", s.Name))
	}
	return strings.Join(names, ", ")
}

// rankConnections returns a server's connections best-first: remote-direct
// HTTPS, then local-direct HTTPS, then any other direct, then relay (which Plex
// bandwidth-throttles and is a poor choice for pulling whole files). Within a
// rank, original order is preserved.
func rankConnections(conns []connectionDTO) []connectionDTO {
	out := make([]connectionDTO, len(conns))
	copy(out, conns)
	sort.SliceStable(out, func(i, j int) bool {
		return connRank(out[i]) < connRank(out[j])
	})
	return out
}

func connRank(c connectionDTO) int {
	https := strings.EqualFold(c.Protocol, "https")
	switch {
	case https && !c.Relay && !c.Local:
		return 0 // remote-direct plex.direct — ideal for a remote container
	case https && !c.Relay && c.Local:
		return 1 // local-direct (reachable only on the server's LAN)
	case !c.Relay:
		return 2 // non-https direct (rare)
	case https && c.Relay:
		return 3 // relay: throttled, last resort
	default:
		return 4
	}
}

// probeIdentity reports whether baseURL serves a Plex /identity endpoint (no
// auth required), using the same TLS-verifying client as real requests so a
// passing probe implies a working data connection.
func probeIdentity(ctx context.Context, hc *http.Client, baseURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/identity", nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode == http.StatusOK
}
