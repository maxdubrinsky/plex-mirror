// Package plex implements source.Source / source.DownloadResolver for a Plex
// Media Server. We talk to Plex's HTTP API directly so each request can carry
// a context.Context — the third-party jrudio/go-plex-client doesn't take one.
// We still reuse its response structs to avoid hand-rolling Plex's JSON shape.
package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gplex "github.com/jrudio/go-plex-client"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

// Config configures a Plex client. Token authenticates every request via the
// X-Plex-Token header. HTTPClient is optional; if nil a sensible default with
// a long-ish timeout is used (Plex libraries can be slow to list).
type Config struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// Client is the concrete type returned by New. It satisfies source.Source and
// source.DownloadResolver. Callers can type-assert to DownloadResolver to get
// download URL resolution; dump.go only needs Source.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New constructs a Plex client. Returns the value typed as source.Source for
// the convenience of dump.go; type-assert to *Client (or to
// source.DownloadResolver) when download URLs are needed.
func New(cfg Config) (source.Source, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("plex: BaseURL is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("plex: Token is required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("plex: invalid BaseURL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("plex: BaseURL must include scheme and host, got %q", cfg.BaseURL)
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.Token,
		http:    hc,
	}, nil
}

// Name implements source.Source.
func (c *Client) Name() string { return "plex" }

// ListLibraries implements source.Source.
func (c *Client) ListLibraries(ctx context.Context) ([]source.Library, error) {
	var out gplex.LibrarySections
	if err := c.getJSON(ctx, "/library/sections", nil, &out); err != nil {
		return nil, err
	}

	libs := make([]source.Library, 0, len(out.MediaContainer.Directory))
	for _, d := range out.MediaContainer.Directory {
		libs = append(libs, source.Library{
			ID:    d.Key,
			Title: d.Title,
			Kind:  libraryKindFromPlex(d.Type),
		})
	}
	return libs, nil
}

// ListItems implements source.Source. Honors opts.Offset/Limit via Plex's
// X-Plex-Container-* headers; an opts.Limit of 0 leaves it to the server.
func (c *Client) ListItems(ctx context.Context, libraryID string, opts source.ListOptions) ([]source.Item, error) {
	if libraryID == "" {
		return nil, fmt.Errorf("plex: libraryID is required")
	}

	// Plex only engages container pagination when X-Plex-Container-Start is
	// present; sending Size alone is silently ignored and the full section is
	// returned (verified live against PMS 1.43). So whenever a Limit is set we
	// must also send Start — defaulting to 0 when no offset was requested.
	headers := http.Header{}
	if opts.Limit > 0 {
		headers.Set("X-Plex-Container-Start", strconv.Itoa(opts.Offset))
		headers.Set("X-Plex-Container-Size", strconv.Itoa(opts.Limit))
	} else if opts.Offset > 0 {
		headers.Set("X-Plex-Container-Start", strconv.Itoa(opts.Offset))
	}

	var out metadataContainer
	path := fmt.Sprintf("/library/sections/%s/all", url.PathEscape(libraryID))
	// Plex's ?title= filters server-side by a per-word prefix match across the
	// whole section (verified live: "crown" matches "The Crown"), so search
	// spans the library rather than just the page we'd otherwise fetch.
	if q := strings.TrimSpace(opts.Query); q != "" {
		path += "?title=" + url.QueryEscape(q)
	}
	if err := c.getJSON(ctx, path, headers, &out); err != nil {
		return nil, err
	}

	items := make([]source.Item, 0, len(out.MediaContainer.Metadata))
	for _, m := range out.MediaContainer.Metadata {
		items = append(items, metadataToItem(m))
	}
	return items, nil
}

// ListChildren implements source.ChildLister. Plex's /children endpoint is
// uniform across the hierarchy: a show yields its seasons, a season yields its
// episodes (and artist→albums, album→tracks). Episode children come back with
// their Media/Part already populated, so metadataToItem stamps size + sNNeNN
// without a per-episode round-trip. Pagination uses the same X-Plex-Container-*
// headers as ListItems.
func (c *Client) ListChildren(ctx context.Context, itemID string, opts source.ListOptions) ([]source.Item, error) {
	if itemID == "" {
		return nil, fmt.Errorf("plex: itemID is required")
	}

	headers := http.Header{}
	if opts.Limit > 0 {
		headers.Set("X-Plex-Container-Start", strconv.Itoa(opts.Offset))
		headers.Set("X-Plex-Container-Size", strconv.Itoa(opts.Limit))
	} else if opts.Offset > 0 {
		headers.Set("X-Plex-Container-Start", strconv.Itoa(opts.Offset))
	}

	var out metadataContainer
	path := fmt.Sprintf("/library/metadata/%s/children", url.PathEscape(itemID))
	if err := c.getJSON(ctx, path, headers, &out); err != nil {
		return nil, err
	}

	items := make([]source.Item, 0, len(out.MediaContainer.Metadata))
	for _, m := range out.MediaContainer.Metadata {
		items = append(items, metadataToItem(m))
	}
	return items, nil
}

// GetMetadata implements source.Source. Returns ErrNotFound if Plex says 404
// or if the response container is empty.
func (c *Client) GetMetadata(ctx context.Context, itemID string) (source.Item, error) {
	if itemID == "" {
		return source.Item{}, fmt.Errorf("plex: itemID is required")
	}

	var out metadataContainer
	path := fmt.Sprintf("/library/metadata/%s", url.PathEscape(itemID))
	if err := c.getJSON(ctx, path, nil, &out); err != nil {
		return source.Item{}, err
	}
	if len(out.MediaContainer.Metadata) == 0 {
		return source.Item{}, fmt.Errorf("plex: metadata %q: %w", itemID, source.ErrNotFound)
	}
	return metadataToItem(out.MediaContainer.Metadata[0]), nil
}

// ResolveDownloadURL implements source.DownloadResolver. Plex hands back bytes
// at Part.Key with ?download=1 — Part.Key already starts with /library/parts/...
// We empirically confirmed that putting the token in the X-Plex-Token header
// (not the query string) preserves 206/Range behavior.
func (c *Client) ResolveDownloadURL(ctx context.Context, itemID string) (*source.DownloadTarget, error) {
	if itemID == "" {
		return nil, fmt.Errorf("plex: itemID is required")
	}

	var out metadataContainer
	path := fmt.Sprintf("/library/metadata/%s", url.PathEscape(itemID))
	if err := c.getJSON(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	if len(out.MediaContainer.Metadata) == 0 {
		return nil, fmt.Errorf("plex: metadata %q: %w", itemID, source.ErrNotFound)
	}

	meta := out.MediaContainer.Metadata[0]
	part, ok := firstPart(meta)
	if !ok {
		return nil, fmt.Errorf("plex: item %q has no parts: %w", itemID, source.ErrNotFound)
	}

	dlURL := c.baseURL + part.Key + "?download=1"
	headers := http.Header{}
	headers.Set("X-Plex-Token", c.token)
	return &source.DownloadTarget{URL: dlURL, Headers: headers}, nil
}

// metadataContainer mirrors only the Plex JSON we consume. We hand-roll it
// instead of reusing gplex.Metadata because the /library/metadata/<id> detail
// endpoint returns `Rating` as an ARRAY of rating tags (IMDb/RT/…), whereas
// gplex.Metadata.Rating is a scalar float64 — decoding the detail response
// into the gplex struct panics with "cannot unmarshal array into ... rating".
// Declaring only the fields we need sidesteps every such shape mismatch.
type metadataContainer struct {
	MediaContainer struct {
		Metadata []plexMetadata `json:"Metadata"`
	} `json:"MediaContainer"`
}

type plexMetadata struct {
	RatingKey            string      `json:"ratingKey"`
	Title                string      `json:"title"`
	Type                 string      `json:"type"`
	Year                 int         `json:"year"`
	GrandparentTitle     string      `json:"grandparentTitle"`
	GrandparentRatingKey string      `json:"grandparentRatingKey"`
	ParentTitle          string      `json:"parentTitle"`
	ParentRatingKey      string      `json:"parentRatingKey"`
	ParentIndex          int         `json:"parentIndex"`
	Index                int         `json:"index"`
	LibrarySectionID     int         `json:"librarySectionID"`
	LibrarySectionTitle  string      `json:"librarySectionTitle"`
	Media                []plexMedia `json:"Media"`
}

type plexMedia struct {
	Part []plexPart `json:"Part"`
}

type plexPart struct {
	Key       string `json:"key"`
	Size      int64  `json:"size"` // int64: large originals exceed int32 (e.g. 60 GB)
	Container string `json:"container"`
}

func firstPart(m plexMetadata) (plexPart, bool) {
	for _, media := range m.Media {
		for _, p := range media.Part {
			return p, true
		}
	}
	return plexPart{}, false
}

func metadataToItem(m plexMetadata) source.Item {
	var size int64
	var container string
	for _, media := range m.Media {
		for _, p := range media.Part {
			size += p.Size
			if container == "" && p.Container != "" {
				container = p.Container
			}
		}
	}
	item := source.Item{
		ID:        m.RatingKey,
		Title:     m.Title,
		Kind:      itemKindFromPlex(m.Type),
		Year:      m.Year,
		Container: container,
		SizeBytes: size,
	}
	// Episodes: pull GrandparentTitle (series) + ParentIndex (season) + Index
	// (episode). Plex always populates these for episode metadata; defensively
	// only read when type is "episode" so we don't accidentally stamp season
	// numbers onto seasons or shows whose Index/ParentIndex mean something else.
	if m.Type == "episode" {
		item.ShowTitle = m.GrandparentTitle
		item.SeasonNumber = int(m.ParentIndex)
		item.EpisodeNumber = int(m.Index)
	}
	// Hierarchy links for breadcrumbs. Plex populates parent on seasons +
	// episodes and grandparent on episodes; both are empty for roots/leaves, so
	// stamping them unconditionally is safe (a movie/show simply gets none).
	item.ParentID = m.ParentRatingKey
	item.ParentTitle = m.ParentTitle
	item.GrandparentID = m.GrandparentRatingKey
	if m.LibrarySectionID > 0 {
		item.LibraryID = strconv.Itoa(m.LibrarySectionID)
		item.LibraryTitle = m.LibrarySectionTitle
	}
	return item
}

func libraryKindFromPlex(t string) source.LibraryKind {
	switch t {
	case "movie":
		return source.LibraryMovies
	case "show":
		return source.LibraryShows
	case "artist", "music":
		return source.LibraryMusic
	default:
		return source.LibraryOther
	}
}

func itemKindFromPlex(t string) source.ItemKind {
	switch t {
	case "movie":
		return source.ItemMovie
	case "show":
		return source.ItemShow
	case "season":
		return source.ItemSeason
	case "episode":
		return source.ItemEpisode
	case "track":
		return source.ItemTrack
	default:
		return source.ItemOther
	}
}

// getJSON issues a GET against c.baseURL + path with extra headers merged in,
// always asking for JSON and authenticating via X-Plex-Token, then JSON-decodes
// the body into out. It maps Plex status codes to source.Err* sentinels.
func (c *Client) getJSON(ctx context.Context, path string, extra http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("plex: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	for k, vv := range extra {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return mapTransportError(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized, http.StatusForbidden:
		// drain so connection can be reused
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("plex: %s: %w", resp.Status, source.ErrAuth)
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("plex: %s %s: %w", req.Method, path, source.ErrNotFound)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("plex: %s %s: %s: %s", req.Method, path, resp.Status, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("plex: decode response: %w", err)
	}
	return nil
}

// mapTransportError classifies pre-response failures. Cancellation propagates
// the original ctx error so callers using errors.Is(err, context.Canceled)
// still work; everything else is wrapped as source.ErrNetwork.
func mapTransportError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return fmt.Errorf("plex: %v: %w", err, source.ErrNetwork)
	}
	return fmt.Errorf("plex: %v: %w", err, source.ErrNetwork)
}
