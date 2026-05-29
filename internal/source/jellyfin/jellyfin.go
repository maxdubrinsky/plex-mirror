// Package jellyfin adapts the Jellyfin HTTP API to source.Source so the
// mirror service can read library inventory and watched state. It deliberately
// does not implement source.DownloadResolver: the mirror already holds the
// bytes locally, and the only reason Jellyfin is wired in is to learn which
// items have been played so the LRU evictor can act on that signal.
package jellyfin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
	jfapi "github.com/sj14/jellyfin-go/api"
)

// Config carries the connection details. HTTPClient is optional; tests use it
// to redirect requests at an httptest server.
type Config struct {
	BaseURL string
	Token   string
	// User optionally pins which user's views/played-state we read, as either a
	// user id or a username. It is only consulted when /Users/Me can't identify
	// the token's owner — i.e. when Token is a server-level API key. With a
	// single-user server it can be left blank.
	User       string
	HTTPClient *http.Client
}

// jellyfinSource is the concrete source.Source. The user ID is resolved
// lazily on the first call that needs it and then cached, because Jellyfin's
// per-user item / view endpoints all require it but /Users/Me is the only way
// to learn it from just an auth token.
type jellyfinSource struct {
	client   *jfapi.APIClient
	userPref string // configured user id/username; consulted only for API keys

	userOnce sync.Once
	userID   string
	userErr  error
}

// New builds a Jellyfin browse adapter. It validates inputs but does not yet
// contact the server; the first ListLibraries/ListItems/GetMetadata call will
// trigger the /Users/Me lookup.
func New(cfg Config) (source.Source, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("jellyfin: BaseURL is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("jellyfin: Token is required")
	}

	apiCfg := jfapi.NewConfiguration()
	apiCfg.Servers = jfapi.ServerConfigurations{{URL: strings.TrimRight(cfg.BaseURL, "/")}}
	if cfg.HTTPClient != nil {
		apiCfg.HTTPClient = cfg.HTTPClient
	}
	// Jellyfin accepts the token either via the MediaBrowser-style Authorization
	// header (quotes around the token are part of the spec) or the X-Emby-Token
	// shortcut. Setting both keeps us compatible with older deployments without
	// trading off newer ones.
	apiCfg.AddDefaultHeader("Authorization", `MediaBrowser Token="`+cfg.Token+`"`)
	apiCfg.AddDefaultHeader("X-Emby-Token", cfg.Token)

	return &jellyfinSource{
		client:   jfapi.NewAPIClient(apiCfg),
		userPref: strings.TrimSpace(cfg.User),
	}, nil
}

func (s *jellyfinSource) Name() string { return "jellyfin" }

func (s *jellyfinSource) resolveUserID(ctx context.Context) (string, error) {
	s.userOnce.Do(func() {
		s.userID, s.userErr = s.lookupUserID(ctx)
	})
	return s.userID, s.userErr
}

// lookupUserID determines the user id whose views/played-state we read. It
// handles both credential kinds Jellyfin issues:
//
//   - A *user* access token identifies its owner via /Users/Me.
//   - A server-level *API key* is owned by no user, so /Users/Me answers
//     400 "Token is not owned by a user." In that case we enumerate users and
//     pick the configured one (s.userPref) or the sole user.
//
// A configured preference short-circuits to the user list, which is the only
// deterministic path for a multi-user server behind an API key.
func (s *jellyfinSource) lookupUserID(ctx context.Context) (string, error) {
	if s.userPref != "" {
		return s.matchUser(ctx, s.userPref)
	}

	user, resp, err := s.client.UserAPI.GetCurrentUser(ctx).Execute()
	switch {
	case err == nil:
		if user == nil || user.Id == nil || *user.Id == "" {
			return "", fmt.Errorf("jellyfin: /Users/Me returned no user id")
		}
		return *user.Id, nil
	case resp != nil && resp.StatusCode == http.StatusBadRequest:
		// Token is an API key (not owned by a user); fall back to enumeration.
		return s.soleUserID(ctx)
	default:
		return "", mapError(resp, err)
	}
}

// matchUser resolves a configured preference (user id, or case-insensitive
// username) against the server's user list.
func (s *jellyfinSource) matchUser(ctx context.Context, pref string) (string, error) {
	users, resp, err := s.client.UserAPI.GetUsers(ctx).Execute()
	if err != nil {
		return "", mapError(resp, err)
	}
	for _, u := range users {
		if u.Id == nil || *u.Id == "" {
			continue
		}
		if *u.Id == pref || strings.EqualFold(nullableString(u.Name), pref) {
			return *u.Id, nil
		}
	}
	return "", fmt.Errorf("jellyfin: configured user %q not found on server (set PLEXMIRROR_JELLYFIN_USER to a valid id or username)", pref)
}

// soleUserID returns the only user on the server. It refuses to guess when more
// than one exists, since the chosen user's played-state drives LRU eviction;
// the operator must disambiguate via PLEXMIRROR_JELLYFIN_USER.
func (s *jellyfinSource) soleUserID(ctx context.Context) (string, error) {
	users, resp, err := s.client.UserAPI.GetUsers(ctx).Execute()
	if err != nil {
		return "", mapError(resp, err)
	}
	switch len(users) {
	case 0:
		return "", fmt.Errorf("jellyfin: token is an API key and the server reports no users")
	case 1:
		if users[0].Id == nil || *users[0].Id == "" {
			return "", fmt.Errorf("jellyfin: user has no id")
		}
		return *users[0].Id, nil
	default:
		names := make([]string, 0, len(users))
		for _, u := range users {
			names = append(names, nullableString(u.Name))
		}
		return "", fmt.Errorf("jellyfin: token is an API key and the server has %d users (%s); set PLEXMIRROR_JELLYFIN_USER to one", len(users), strings.Join(names, ", "))
	}
}

func (s *jellyfinSource) ListLibraries(ctx context.Context) ([]source.Library, error) {
	userID, err := s.resolveUserID(ctx)
	if err != nil {
		return nil, err
	}

	result, resp, err := s.client.UserViewsAPI.GetUserViews(ctx).UserId(userID).Execute()
	if err != nil {
		return nil, mapError(resp, err)
	}
	if result == nil {
		return nil, nil
	}

	libs := make([]source.Library, 0, len(result.Items))
	for _, it := range result.Items {
		libs = append(libs, source.Library{
			ID:    derefString(it.Id),
			Title: nullableString(it.Name),
			Kind:  collectionKind(it.CollectionType),
		})
	}
	return libs, nil
}

func (s *jellyfinSource) ListItems(ctx context.Context, libraryID string, opts source.ListOptions) ([]source.Item, error) {
	if strings.TrimSpace(libraryID) == "" {
		return nil, fmt.Errorf("jellyfin: libraryID is required")
	}

	userID, err := s.resolveUserID(ctx)
	if err != nil {
		return nil, err
	}

	// Recursive=true so that callers asking for a movie/show/music library get
	// the leaf items (movies, series, tracks) instead of just the immediate
	// folder children. EnableUserData is required for the Played field to be
	// populated.
	req := s.client.ItemsAPI.GetItems(ctx).
		UserId(userID).
		ParentId(libraryID).
		Recursive(true).
		EnableUserData(true)

	if opts.Offset > 0 {
		req = req.StartIndex(int32(opts.Offset))
	}
	if opts.Limit > 0 {
		req = req.Limit(int32(opts.Limit))
	}
	// SearchTerm pushes title search to the server so it spans the whole
	// library rather than the current page.
	if q := strings.TrimSpace(opts.Query); q != "" {
		req = req.SearchTerm(q)
	}

	result, resp, err := req.Execute()
	if err != nil {
		return nil, mapError(resp, err)
	}
	if result == nil {
		return nil, nil
	}

	items := make([]source.Item, 0, len(result.Items))
	for _, it := range result.Items {
		items = append(items, toItem(it))
	}
	return items, nil
}

// ListChildren implements source.ChildLister: the direct children of an item
// (show → seasons, season → episodes). Recursive=false so only the immediate
// level comes back, unlike ListItems which flattens a whole library.
func (s *jellyfinSource) ListChildren(ctx context.Context, itemID string, opts source.ListOptions) ([]source.Item, error) {
	if strings.TrimSpace(itemID) == "" {
		return nil, fmt.Errorf("jellyfin: itemID is required")
	}

	userID, err := s.resolveUserID(ctx)
	if err != nil {
		return nil, err
	}

	req := s.client.ItemsAPI.GetItems(ctx).
		UserId(userID).
		ParentId(itemID).
		Recursive(false).
		EnableUserData(true)

	if opts.Offset > 0 {
		req = req.StartIndex(int32(opts.Offset))
	}
	if opts.Limit > 0 {
		req = req.Limit(int32(opts.Limit))
	}

	result, resp, err := req.Execute()
	if err != nil {
		return nil, mapError(resp, err)
	}
	if result == nil {
		return nil, nil
	}

	items := make([]source.Item, 0, len(result.Items))
	for _, it := range result.Items {
		items = append(items, toItem(it))
	}
	return items, nil
}

func (s *jellyfinSource) GetMetadata(ctx context.Context, itemID string) (source.Item, error) {
	if strings.TrimSpace(itemID) == "" {
		return source.Item{}, fmt.Errorf("jellyfin: itemID is required")
	}

	userID, err := s.resolveUserID(ctx)
	if err != nil {
		return source.Item{}, err
	}

	item, resp, err := s.client.UserLibraryAPI.GetItem(ctx, itemID).UserId(userID).Execute()
	if err != nil {
		return source.Item{}, mapError(resp, err)
	}
	if item == nil {
		return source.Item{}, fmt.Errorf("jellyfin: empty response for item %q: %w", itemID, source.ErrNotFound)
	}
	return toItem(*item), nil
}

func toItem(b jfapi.BaseItemDto) source.Item {
	out := source.Item{
		ID:        derefString(b.Id),
		Title:     nullableString(b.Name),
		Kind:      itemKind(b.Type),
		Container: nullableString(b.Container),
	}
	if b.ProductionYear.IsSet() {
		if v := b.ProductionYear.Get(); v != nil {
			out.Year = int(*v)
		}
	}
	if b.UserData.IsSet() {
		if ud := b.UserData.Get(); ud != nil && ud.Played != nil {
			played := *ud.Played
			out.Played = &played
		}
	}
	// Hierarchy links for breadcrumbs + the show/season/episode fields the
	// download layout wants. Jellyfin spells these out per kind: an episode
	// carries Series* (show) + Season* + ParentId (its season); a season carries
	// Series* (show) as its parent.
	switch out.Kind {
	case source.ItemEpisode:
		out.ShowTitle = nullableString(b.SeriesName)
		out.GrandparentID = firstNonEmpty(nullableString(b.SeriesId), "")
		out.ParentID = firstNonEmpty(nullableString(b.SeasonId), nullableString(b.ParentId))
		out.ParentTitle = nullableString(b.SeasonName)
		out.SeasonNumber = nullableInt32(b.ParentIndexNumber)
		out.EpisodeNumber = nullableInt32(b.IndexNumber)
	case source.ItemSeason:
		out.ParentID = firstNonEmpty(nullableString(b.SeriesId), nullableString(b.ParentId))
		out.ParentTitle = nullableString(b.SeriesName)
		out.SeasonNumber = nullableInt32(b.IndexNumber)
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func collectionKind(ct jfapi.NullableCollectionType) source.LibraryKind {
	if !ct.IsSet() {
		return source.LibraryOther
	}
	v := ct.Get()
	if v == nil {
		return source.LibraryOther
	}
	switch *v {
	case jfapi.COLLECTIONTYPE_MOVIES:
		return source.LibraryMovies
	case jfapi.COLLECTIONTYPE_TVSHOWS:
		return source.LibraryShows
	case jfapi.COLLECTIONTYPE_MUSIC:
		return source.LibraryMusic
	default:
		return source.LibraryOther
	}
}

func itemKind(k *jfapi.BaseItemKind) source.ItemKind {
	if k == nil {
		return source.ItemOther
	}
	switch *k {
	case jfapi.BASEITEMKIND_MOVIE:
		return source.ItemMovie
	case jfapi.BASEITEMKIND_SERIES:
		return source.ItemShow
	case jfapi.BASEITEMKIND_SEASON:
		return source.ItemSeason
	case jfapi.BASEITEMKIND_EPISODE:
		return source.ItemEpisode
	case jfapi.BASEITEMKIND_AUDIO, jfapi.BASEITEMKIND_AUDIO_BOOK, jfapi.BASEITEMKIND_MUSIC_VIDEO:
		return source.ItemTrack
	default:
		return source.ItemOther
	}
}

func nullableString(ns jfapi.NullableString) string {
	if !ns.IsSet() {
		return ""
	}
	if v := ns.Get(); v != nil {
		return *v
	}
	return ""
}

func nullableInt32(ni jfapi.NullableInt32) int {
	if !ni.IsSet() {
		return 0
	}
	if v := ni.Get(); v != nil {
		return int(*v)
	}
	return 0
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// mapError converts a generated-client error into one of the source.Err*
// sentinels. The OpenAPI client returns the *http.Response even on transport
// success-but-HTTP-error, so we prefer the status code when available and
// fall back to wrapping the raw error as a network failure.
func mapError(resp *http.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("jellyfin: %w", source.ErrAuth)
		case http.StatusNotFound:
			return fmt.Errorf("jellyfin: %w", source.ErrNotFound)
		}
	}
	// Surface url.Error / DNS / connection-refused as network failures so
	// callers can retry without inspecting concrete types.
	var urlErr interface{ Timeout() bool }
	if errors.As(err, &urlErr) {
		return fmt.Errorf("jellyfin: %w: %v", source.ErrNetwork, err)
	}
	if resp == nil {
		return fmt.Errorf("jellyfin: %w: %v", source.ErrNetwork, err)
	}
	return fmt.Errorf("jellyfin: %s: %s", resp.Status, errorDetail(err))
}

// errorDetail extracts a human-readable reason from a generated-client error.
// The client's own Error() runs ProblemDetails through a formatter that %s-prints
// NullableString fields, yielding "%!s(*string=0x...)" noise, so we prefer the
// raw JSON body it carries on GenericOpenAPIError.
func errorDetail(err error) string {
	var apiErr *jfapi.GenericOpenAPIError
	if errors.As(err, &apiErr) {
		if body := apiErr.Body(); len(body) > 0 {
			return strings.TrimSpace(string(body))
		}
	}
	return err.Error()
}
