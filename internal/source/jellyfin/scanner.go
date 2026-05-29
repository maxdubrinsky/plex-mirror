package jellyfin

import (
	"context"
	"fmt"
	"strings"

	jfapi "github.com/sj14/jellyfin-go/api"
)

// Scanner triggers a Jellyfin library scan after the download engine drops a
// new file under the media root. It deliberately mirrors the Config shape of
// New() so callers can build both from the same env vars.
//
// The endpoint requires an admin token; if the configured token is read-only
// the request returns 403 and we surface that as ErrAuth via the source.Err*
// sentinel set so the engine can decide whether to keep retrying.
type Scanner struct {
	client *jfapi.APIClient
}

// NewScanner builds a Jellyfin admin scanner. Validates inputs but does not
// hit the server.
func NewScanner(cfg Config) (*Scanner, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("jellyfin scanner: BaseURL is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("jellyfin scanner: Token is required")
	}

	apiCfg := jfapi.NewConfiguration()
	apiCfg.Servers = jfapi.ServerConfigurations{{URL: strings.TrimRight(cfg.BaseURL, "/")}}
	if cfg.HTTPClient != nil {
		apiCfg.HTTPClient = cfg.HTTPClient
	}
	apiCfg.AddDefaultHeader("Authorization", `MediaBrowser Token="`+cfg.Token+`"`)
	apiCfg.AddDefaultHeader("X-Emby-Token", cfg.Token)

	return &Scanner{client: jfapi.NewAPIClient(apiCfg)}, nil
}

// Refresh kicks off a full library scan. Jellyfin returns 204 No Content on
// acceptance; the actual scan runs async on the server. Returns an error
// wrapping source.Err* sentinels on auth / network failure.
func (s *Scanner) Refresh(ctx context.Context) error {
	resp, err := s.client.LibraryAPI.RefreshLibrary(ctx).Execute()
	if err != nil {
		return mapError(resp, err)
	}
	if resp != nil && resp.StatusCode >= 400 {
		return fmt.Errorf("jellyfin scanner: refresh returned %s", resp.Status)
	}
	return nil
}

// Compile-time assertion that *Scanner satisfies the simple interface the
// download engine consumes — kept here to avoid an import cycle from the
// download package.
var _ interface {
	Refresh(ctx context.Context) error
} = (*Scanner)(nil)
