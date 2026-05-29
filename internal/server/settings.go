package server

import (
	"errors"
	"net/http"

	"github.com/maxdubrinsky/plex-mirror/internal/server/views"
	"github.com/maxdubrinsky/plex-mirror/internal/service"
)

// handleSettingsPage renders the full settings page with the current effective
// (secret-free) configuration.
func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	vm := views.SettingsVM{}
	cv, err := s.svc.ConfigView(r.Context())
	if err != nil {
		vm.Err = err.Error()
	}
	vm.Config = cv
	render(w, r, views.SettingsPage(s.shell(r.Context(), "Settings", "settings", "", ""), vm))
}

// handleSettingsSave validates + persists the form and live-reloads the service.
// It always re-renders the form fragment (200) with a saved/error banner so an
// invalid value is shown inline rather than as an HTTP error. The fragment
// reflects the post-reload effective config.
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	up := service.ConfigUpdate{
		PlexURL:            r.FormValue("plex_url"),
		PlexServer:         r.FormValue("plex_server"),
		PlexClientID:       r.FormValue("plex_client_id"),
		PlexToken:          r.FormValue("plex_token"),
		ClearPlexToken:     r.FormValue("clear_plex_token") != "",
		JellyfinURL:        r.FormValue("jellyfin_url"),
		JellyfinUser:       r.FormValue("jellyfin_user"),
		JellyfinToken:      r.FormValue("jellyfin_token"),
		ClearJellyfinToken: r.FormValue("clear_jellyfin_token") != "",
		StorageHardCap:     r.FormValue("storage_hard_cap"),
		StorageSoftCap:     r.FormValue("storage_soft_cap"),

		DownloadConcurrency: r.FormValue("download_concurrency"),
		DownloadPollEvery:   r.FormValue("download_poll_every"),
		DownloadBuffer:      r.FormValue("download_buffer"),
	}

	vm := views.SettingsVM{}
	switch err := s.svc.ApplySettings(ctx, up); {
	case err == nil:
		vm.Saved = true
	case errors.Is(err, service.ErrReloadAfterSave):
		// Persisted, but the live swap failed — be honest rather than reporting a
		// flat error for settings that did save and will apply on restart.
		vm.SavedNotLive = true
		vm.Warn = err.Error()
	default:
		vm.Err = err.Error()
	}

	// A successful (or persisted-but-not-live) save can change which sources/
	// libraries exist, so drop the cached sidebar tree to rebuild it next render.
	if vm.Saved || vm.SavedNotLive {
		s.nav.clear()
	}

	// Re-read the (possibly just-reloaded) effective config so the form shows the
	// live state, including updated secret badges. A re-read failure must not mask
	// a successful save: only surface it when we're not already reporting success.
	cv, cerr := s.svc.ConfigView(ctx)
	if cerr != nil && vm.Err == "" && !vm.Saved && !vm.SavedNotLive {
		vm.Err = cerr.Error()
	}
	vm.Config = cv
	render(w, r, views.SettingsForm(vm))
}
