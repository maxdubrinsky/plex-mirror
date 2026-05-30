package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/settings"
)

// ErrReloadAfterSave means the new settings were validated and persisted, but
// applying them to the running service failed (e.g. a bad URL that only fails at
// client construction). The stored config will take effect on the next restart;
// the live runtime is still on the previous config. Callers should report this
// distinctly from a validation failure (where nothing was saved).
var ErrReloadAfterSave = errors.New("settings saved, but applying them live failed")

// ConfigView is the read-only projection of the running configuration for the
// portal settings form and the MCP get_config tool. Secret values are NEVER
// included — only booleans saying whether a credential is set or unreadable.
type ConfigView struct {
	// Env-only, read-only context.
	MediaRoot    string `json:"media_root"`
	SecretKeySet bool   `json:"secret_key_set"` // PLEXMIRROR_SECRET_KEY set → secrets encrypted at rest
	AuthEnabled  bool   `json:"auth_enabled"`

	// Editable, non-secret. Strings are the current effective values; caps/buffer
	// round-trip the operator's own "10G"-style string when one was stored.
	PlexURL             string `json:"plex_url"`
	PlexServer          string `json:"plex_server"`
	PlexClientID        string `json:"plex_client_id"`
	JellyfinURL         string `json:"jellyfin_url"`
	JellyfinUser        string `json:"jellyfin_user"`
	MoviesDir           string `json:"movies_dir"`
	ShowsDir            string `json:"shows_dir"`
	OtherDir            string `json:"other_dir"`
	StorageHardCap      string `json:"storage_hard_cap"`
	StorageSoftCap      string `json:"storage_soft_cap"`
	DownloadConcurrency string `json:"download_concurrency"`
	DownloadPollEvery   string `json:"download_poll_every"`
	DownloadBuffer      string `json:"download_buffer"`
	HealthCheckEvery    string `json:"health_check_every"`

	// Secrets: whether a credential is in effect (never the value). *Unreadable
	// distinguishes "stored but undecryptable with the current key" (rotated/
	// misconfigured PLEXMIRROR_SECRET_KEY) from a genuinely absent token.
	PlexTokenSet            bool `json:"plex_token_set"`
	PlexTokenUnreadable     bool `json:"plex_token_unreadable"`
	JellyfinTokenSet        bool `json:"jellyfin_token_set"`
	JellyfinTokenUnreadable bool `json:"jellyfin_token_unreadable"`
}

// ConfigUpdate is the portal settings form, mapped 1:1 from the POST body. The
// Clear* flags wipe a stored credential; a non-empty token field replaces it;
// an empty token field with Clear unset leaves it unchanged (so the operator
// never has to re-enter a token just to edit, say, the storage cap).
type ConfigUpdate struct {
	PlexURL        string
	PlexServer     string
	PlexClientID   string
	PlexToken      string
	ClearPlexToken bool

	JellyfinURL        string
	JellyfinUser       string
	JellyfinToken      string
	ClearJellyfinToken bool

	MoviesDir string
	ShowsDir  string
	OtherDir  string

	StorageHardCap string
	StorageSoftCap string

	DownloadConcurrency string
	DownloadPollEvery   string
	DownloadBuffer      string
	HealthCheckEvery    string
}

// ConfigView returns the current effective configuration for rendering the
// settings form. Even when the stored settings are invalid it returns a view
// (falling back to the env base) so the operator can correct them rather than
// being locked out.
func (s *Service) ConfigView(ctx context.Context) (ConfigView, error) {
	vals, err := s.settings.GetAll(ctx)
	if err != nil {
		return ConfigView{}, err
	}
	state, err := s.settings.SecretState(ctx)
	if err != nil {
		return ConfigView{}, err
	}
	eff, err := settings.Effective(s.baseCfg, vals)
	if err != nil {
		eff = s.baseCfg
	}

	cv := ConfigView{
		MediaRoot:    s.baseCfg.MediaRoot,
		SecretKeySet: s.settings.Encrypts(),
		AuthEnabled:  s.baseCfg.AuthToken != "",

		PlexURL:      eff.PlexURL,
		PlexServer:   eff.PlexServer,
		PlexClientID: eff.PlexClientID,
		JellyfinURL:  eff.JellyfinURL,
		JellyfinUser: eff.JellyfinUser,
		MoviesDir:    eff.MoviesDir,
		ShowsDir:     eff.ShowsDir,
		OtherDir:     eff.OtherDir,

		StorageHardCap:      capView(vals, settings.KeyStorageHardCap, eff.StorageHardCapBytes),
		StorageSoftCap:      capView(vals, settings.KeyStorageSoftCap, eff.StorageSoftCapBytes),
		DownloadConcurrency: strconv.Itoa(eff.DownloadConcurrency),
		DownloadPollEvery:   eff.DownloadPollEvery.String(),
		DownloadBuffer:      bufferView(vals, eff.DownloadBufferBytes),
		HealthCheckEvery:    eff.HealthCheckEvery.String(),
	}
	cv.PlexTokenSet, cv.PlexTokenUnreadable = tokenBadge(state, settings.KeyPlexToken, eff.PlexToken)
	cv.JellyfinTokenSet, cv.JellyfinTokenUnreadable = tokenBadge(state, settings.KeyJellyfinToken, eff.JellyfinToken)
	return cv, nil
}

// tokenBadge resolves a credential's display state. A stored row's state wins
// (set / unset / unreadable); with no row, fall back to whether the env base
// supplies a token.
func tokenBadge(state map[string]string, key, effToken string) (set, unreadable bool) {
	switch state[key] {
	case "set":
		return true, false
	case "unreadable":
		return false, true
	case "unset":
		return false, false
	default: // no stored row → env-derived
		return effToken != "", false
	}
}

// capView shows the operator's own size string when one is stored (so "10G"
// round-trips instead of becoming 10737418240), else the effective byte count
// as a plain ParseSize-able integer, else "" when unset. For caps an empty
// stored value genuinely means 0/uncapped, so blank is correct.
func capView(vals map[string]string, key string, effBytes int64) string {
	if raw, ok := vals[key]; ok {
		return raw
	}
	if effBytes <= 0 {
		return ""
	}
	return strconv.FormatInt(effBytes, 10)
}

// bufferView is like capView but for the copy buffer, where a present-but-empty
// stored value KEEPS the base default (Effective's non-empty guard) rather than
// meaning 0. So an empty stored value must still display the running buffer,
// not blank — otherwise the form would misreport the buffer actually in use.
func bufferView(vals map[string]string, effBytes int64) string {
	if raw, ok := vals[settings.KeyDownloadBuffer]; ok && strings.TrimSpace(raw) != "" {
		return raw
	}
	if effBytes <= 0 {
		return ""
	}
	return strconv.FormatInt(effBytes, 10)
}

// ApplySettings validates the form against the current config, persists it
// atomically, and live-reloads the service (no restart). On a validation error
// nothing is persisted and the prior config keeps running. If persistence
// succeeds but the live reload fails, it returns ErrReloadAfterSave wrapping the
// reload error — the settings ARE saved (and take effect on restart), only the
// live swap failed.
//
// Note: the form posts every non-secret field, so saving "pins" the current
// effective values into the DB — the DB layer becomes the source of truth for
// those fields after the first save, and later env changes to them no longer
// take effect (by design: the portal owns config once an operator uses it).
func (s *Service) ApplySettings(ctx context.Context, up ConfigUpdate) error {
	cur, err := s.settings.GetAll(ctx)
	if err != nil {
		return err
	}

	// Non-secret keys are always posted by the form; secrets only when changing.
	toSet := map[string]string{
		settings.KeyPlexURL:             up.PlexURL,
		settings.KeyPlexServer:          up.PlexServer,
		settings.KeyPlexClientID:        up.PlexClientID,
		settings.KeyJellyfinURL:         up.JellyfinURL,
		settings.KeyJellyfinUser:        up.JellyfinUser,
		settings.KeyLibraryMoviesDir:    strings.TrimSpace(up.MoviesDir),
		settings.KeyLibraryShowsDir:     strings.TrimSpace(up.ShowsDir),
		settings.KeyLibraryOtherDir:     strings.TrimSpace(up.OtherDir),
		settings.KeyStorageHardCap:      strings.TrimSpace(up.StorageHardCap),
		settings.KeyStorageSoftCap:      strings.TrimSpace(up.StorageSoftCap),
		settings.KeyDownloadConcurrency: strings.TrimSpace(up.DownloadConcurrency),
		settings.KeyDownloadPollEvery:   strings.TrimSpace(up.DownloadPollEvery),
		settings.KeyDownloadBuffer:      strings.TrimSpace(up.DownloadBuffer),
		settings.KeyHealthCheckEvery:    strings.TrimSpace(up.HealthCheckEvery),
	}
	applySecretToSet(toSet, settings.KeyPlexToken, up.PlexToken, up.ClearPlexToken)
	applySecretToSet(toSet, settings.KeyJellyfinToken, up.JellyfinToken, up.ClearJellyfinToken)

	// Validate the prospective merged config (current ∪ form) before writing.
	next := maps.Clone(cur)
	maps.Copy(next, toSet)
	if _, err := settings.Effective(s.baseCfg, next); err != nil {
		return err
	}

	// Persist atomically: all keys or none.
	if err := s.settings.SetMany(ctx, toSet); err != nil {
		return err
	}

	// Live re-init on a detached context so a client disconnect mid-discovery
	// can't abort the reload after the settings were already written.
	rctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := s.Reload(rctx); err != nil {
		return fmt.Errorf("%w: %w", ErrReloadAfterSave, err)
	}
	return nil
}

// applySecretToSet folds a secret form field into the to-write map only when it
// should change: clear → "", a new non-empty token → that value. A blank field
// with clear unset leaves the key out, so SetMany never rewrites (and thus never
// clobbers) the stored token.
func applySecretToSet(toSet map[string]string, key, newVal string, clear bool) {
	switch {
	case clear:
		toSet[key] = ""
	case newVal != "":
		toSet[key] = newVal
	}
}
