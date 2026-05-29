package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSettingsPageRenders(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Settings", `name="plex_url"`, `name="storage_hard_cap"`, `name="plex_token"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
	// No secret key configured in the test → the plaintext warning must show.
	if !strings.Contains(body, "PLEXMIRROR_SECRET_KEY is not set") {
		t.Errorf("expected plaintext-storage warning on the settings page")
	}
}

func TestSettingsSaveRedactsToken(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	// Only a token (no URL/server) so the live reload doesn't attempt Plex
	// discovery (no network in tests).
	form := url.Values{
		"plex_token":           {"super-secret-123"},
		"download_concurrency": {""},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "super-secret-123") {
		t.Fatalf("token echoed back in the response:\n%s", body)
	}
	if !strings.Contains(body, "Saved and applied") {
		t.Errorf("expected success banner, got:\n%s", body)
	}
	// The Plex token badge should now read "set".
	if !strings.Contains(body, "badge-ready") {
		t.Errorf("expected token 'set' badge after save")
	}
}

func TestSettingsSaveReloadFailureShowsWarnNotError(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	// Schemeless URL passes validation but fails the live plex client build, so
	// the settings save but the live reload doesn't — the banner must say so
	// (warn), not report a flat failure, and must not echo the token.
	form := url.Values{
		"plex_url":   {"noscheme-host"},
		"plex_token": {"tok-xyz"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "take effect on the next restart") {
		t.Fatalf("expected 'saved but not live' warn banner, got:\n%s", body)
	}
	if strings.Contains(body, "Saved and applied live") {
		t.Fatalf("should not claim live success when reload failed")
	}
	if strings.Contains(body, "tok-xyz") {
		t.Fatalf("token leaked into response:\n%s", body)
	}
}

func TestSettingsSaveValidationErrorInline(t *testing.T) {
	h, _, _ := newTestServer(t, "")
	form := url.Values{
		"storage_hard_cap": {"5G"},
		"storage_soft_cap": {"9G"}, // soft > hard → rejected
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (inline error)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "soft cap") || !strings.Contains(body, "must be") {
		t.Fatalf("expected soft>hard validation error inline, got:\n%s", body)
	}
	if strings.Contains(body, "Saved and applied") {
		t.Fatalf("should not report success on a validation failure")
	}
}
