package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/settings"
)

// newConfigService builds a Service via the real New() path (env→settings→
// runtime) over a fresh migrated DB, so the settings store and live reload are
// exercised end to end. MediaRoot is a temp dir so StorageStats' statfs works.
func newConfigService(t *testing.T, cfg *config.Config) (*Service, *db.Store) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if cfg.MediaRoot == "" {
		cfg.MediaRoot = t.TempDir()
	}
	svc, err := New(cfg, store)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	return svc, store
}

func TestConfigViewRedactsSecrets(t *testing.T) {
	svc, _ := newConfigService(t, &config.Config{PlexToken: "env-secret-token"})
	cv, err := svc.ConfigView(context.Background())
	if err != nil {
		t.Fatalf("ConfigView: %v", err)
	}
	if !cv.PlexTokenSet {
		t.Errorf("PlexTokenSet = false, want true (env token present)")
	}
	// The serialized view (what MCP get_config returns) must never carry the
	// token value.
	b, _ := json.Marshal(cv)
	if strings.Contains(string(b), "env-secret-token") {
		t.Fatalf("token leaked into ConfigView JSON:\n%s", b)
	}
}

func TestApplySettingsLiveReloadStorageCaps(t *testing.T) {
	svc, _ := newConfigService(t, &config.Config{})
	ctx := context.Background()

	// Baseline: no caps.
	if st, _ := svc.StorageStats(ctx); st.HardCapBytes != 0 {
		t.Fatalf("baseline hard cap = %d, want 0", st.HardCapBytes)
	}

	if err := svc.ApplySettings(ctx, ConfigUpdate{
		StorageHardCap: "10G",
		StorageSoftCap: "8G",
	}); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	// The storage manager was rebuilt live: StorageStats now reflects the caps
	// without a restart.
	st, err := svc.StorageStats(ctx)
	if err != nil {
		t.Fatalf("StorageStats: %v", err)
	}
	if st.HardCapBytes != 10<<30 || st.SoftCapBytes != 8<<30 {
		t.Fatalf("post-reload caps = %d/%d, want 10G/8G", st.HardCapBytes, st.SoftCapBytes)
	}

	// And the values persist to the settings table for the next boot.
	cv, _ := svc.ConfigView(ctx)
	if cv.StorageHardCap != "10G" || cv.StorageSoftCap != "8G" {
		t.Fatalf("ConfigView caps = %q/%q, want 10G/8G", cv.StorageHardCap, cv.StorageSoftCap)
	}
}

func TestApplySettingsInvalidNotPersisted(t *testing.T) {
	svc, _ := newConfigService(t, &config.Config{})
	ctx := context.Background()

	// soft > hard must be rejected and nothing persisted / reloaded.
	if err := svc.ApplySettings(ctx, ConfigUpdate{
		StorageHardCap: "5G",
		StorageSoftCap: "9G",
	}); err == nil {
		t.Fatal("ApplySettings(soft>hard): want error, got nil")
	}

	st, _ := svc.StorageStats(ctx)
	if st.HardCapBytes != 0 || st.SoftCapBytes != 0 {
		t.Fatalf("caps changed despite validation failure: %d/%d", st.HardCapBytes, st.SoftCapBytes)
	}
	cv, _ := svc.ConfigView(ctx)
	if cv.StorageHardCap != "" || cv.StorageSoftCap != "" {
		t.Fatalf("invalid caps persisted: %q/%q", cv.StorageHardCap, cv.StorageSoftCap)
	}
}

func TestApplySettingsSecretEncryptedAndClearable(t *testing.T) {
	svc, store := newConfigService(t, &config.Config{SecretKey: "master-key"})
	ctx := context.Background()

	if err := svc.ApplySettings(ctx, ConfigUpdate{PlexToken: "live-token"}); err != nil {
		t.Fatalf("ApplySettings set token: %v", err)
	}
	cv, _ := svc.ConfigView(ctx)
	if !cv.PlexTokenSet {
		t.Fatal("PlexTokenSet = false after setting a token")
	}

	// Stored at rest as ciphertext, never the plaintext token.
	var raw string
	if err := store.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, settings.KeyPlexToken).Scan(&raw); err != nil {
		t.Fatalf("read raw token row: %v", err)
	}
	if strings.Contains(raw, "live-token") {
		t.Fatalf("token stored in plaintext: %q", raw)
	}

	// Clearing it via the checkbox flag wipes the credential.
	if err := svc.ApplySettings(ctx, ConfigUpdate{ClearPlexToken: true}); err != nil {
		t.Fatalf("ApplySettings clear token: %v", err)
	}
	cv2, _ := svc.ConfigView(ctx)
	if cv2.PlexTokenSet {
		t.Fatal("PlexTokenSet = true after clear, want false")
	}
}

func TestApplySettingsReloadFailureReportsSavedNotLive(t *testing.T) {
	svc, _ := newConfigService(t, &config.Config{})
	ctx := context.Background()

	// A schemeless Plex URL passes settings validation (no URL syntax check) but
	// fails plex.New during the live rebuild → settings persist, reload fails.
	err := svc.ApplySettings(ctx, ConfigUpdate{
		PlexURL:   "noscheme-host",
		PlexToken: "tok",
	})
	if !errors.Is(err, ErrReloadAfterSave) {
		t.Fatalf("err = %v, want ErrReloadAfterSave", err)
	}
	// The values were persisted despite the failed live swap.
	cv, _ := svc.ConfigView(ctx)
	if cv.PlexURL != "noscheme-host" {
		t.Fatalf("PlexURL = %q, want persisted 'noscheme-host'", cv.PlexURL)
	}
	// The live runtime stayed on the old (sourceless) config — Plex never came up.
	for _, src := range svc.ListSources() {
		if src.Name == "plex" {
			t.Fatalf("plex source should not be live after a failed reload: %+v", svc.ListSources())
		}
	}
}

// TestStartThenReloadCyclesWorkers exercises the real worker lifecycle: Start
// launches a sweeper that blocks on a long ticker, and each Reload must cancel +
// wait out that generation and relaunch without deadlocking (Reload holds s.mu
// across the wait; the workers must not need s.mu). Run under -race this also
// covers the atomic runtime swap against the live workers.
func TestStartThenReloadCyclesWorkers(t *testing.T) {
	svc, _ := newConfigService(t, &config.Config{StorageSweepEvery: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Start(ctx) // launches a sweeper blocked on a 1h ticker
	for i := 0; i < 2; i++ {
		if err := svc.Reload(ctx); err != nil {
			t.Fatalf("Reload #%d after Start: %v", i+1, err)
		}
	}
}

func TestReloadResetsStaleDownloadRow(t *testing.T) {
	svc, store := newConfigService(t, &config.Config{})
	ctx := context.Background()

	// Simulate a row left mid-download (as a cancelled in-flight transfer would).
	if _, err := store.ExecContext(ctx, `
		INSERT INTO items (source, source_key, title, status, bytes_done, started_at)
		VALUES ('plex','rk-stale','Stuck','downloading',123, unixepoch())`); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	if err := svc.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	var status string
	if err := store.QueryRowContext(ctx,
		`SELECT status FROM items WHERE source_key='rk-stale'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Fatalf("stale 'downloading' row status = %q after reload, want 'queued' (engine-independent reset)", status)
	}
}

func TestApplySettingsTokenUnchangedWhenBlank(t *testing.T) {
	svc, _ := newConfigService(t, &config.Config{SecretKey: "k"})
	ctx := context.Background()

	if err := svc.ApplySettings(ctx, ConfigUpdate{PlexToken: "keep-me"}); err != nil {
		t.Fatalf("set token: %v", err)
	}
	// A later save that leaves the token field blank (and clear unchecked) must
	// not wipe the stored token — the common "edit a cap, don't re-type secrets"
	// case.
	if err := svc.ApplySettings(ctx, ConfigUpdate{StorageHardCap: "1G"}); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	cv, _ := svc.ConfigView(ctx)
	if !cv.PlexTokenSet {
		t.Fatal("blank token field wiped the stored token; want preserved")
	}
}
