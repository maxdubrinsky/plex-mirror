package settings

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
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

var testSalt = []byte("0123456789abcdef")

func TestCryptoRoundTrip(t *testing.T) {
	key := deriveKey("hunter2", testSalt)
	if len(key) != 32 {
		t.Fatalf("derived key len = %d, want 32", len(key))
	}

	enc, err := encryptValue(key, "s3cr3t-token")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, cipherPrefix) {
		t.Fatalf("ciphertext missing %q prefix: %q", cipherPrefix, enc)
	}
	if strings.Contains(enc, "s3cr3t-token") {
		t.Fatalf("plaintext leaked into ciphertext: %q", enc)
	}

	got, err := decryptValue(key, enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "s3cr3t-token" {
		t.Fatalf("roundtrip = %q, want s3cr3t-token", got)
	}

	// Nonce is random, so two encryptions of the same plaintext differ.
	enc2, _ := encryptValue(key, "s3cr3t-token")
	if enc == enc2 {
		t.Fatal("expected distinct ciphertexts (random nonce), got identical")
	}
}

func TestCryptoNoKeyIsPlaintext(t *testing.T) {
	if k := deriveKey("", testSalt); k != nil {
		t.Fatalf("empty passphrase derived key = %v, want nil", k)
	}
	enc, err := encryptValue(nil, "plain")
	if err != nil {
		t.Fatalf("encrypt nil key: %v", err)
	}
	if enc != "plain" {
		t.Fatalf("nil-key encrypt = %q, want passthrough 'plain'", enc)
	}
	got, err := decryptValue(nil, "plain")
	if err != nil || got != "plain" {
		t.Fatalf("nil-key decrypt = %q, %v; want plain", got, err)
	}
}

func TestCryptoEncryptedValueNeedsKey(t *testing.T) {
	enc, _ := encryptValue(deriveKey("key-a", testSalt), "secret")
	// No key but value is gcm: → error, never a silent empty/garbage read.
	if _, err := decryptValue(nil, enc); err == nil {
		t.Fatal("decrypt gcm value with nil key: want error, got nil")
	}
	// Wrong key → error.
	if _, err := decryptValue(deriveKey("key-b", testSalt), enc); err == nil {
		t.Fatal("decrypt with wrong key: want error, got nil")
	}
}

func TestStoreSecretEncryptedAtRest(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	s := NewStore(store, "passphrase")
	if !s.Encrypts() {
		t.Fatal("Encrypts() = false with a passphrase, want true")
	}

	if err := s.Set(ctx, KeyPlexToken, "tok-123"); err != nil {
		t.Fatalf("Set secret: %v", err)
	}

	// The raw stored value must be ciphertext, not the plaintext token.
	var raw string
	var isSecret int
	if err := store.QueryRowContext(ctx,
		`SELECT value, is_secret FROM settings WHERE key = ?`, KeyPlexToken).
		Scan(&raw, &isSecret); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if isSecret != 1 {
		t.Errorf("is_secret = %d, want 1", isSecret)
	}
	if strings.Contains(raw, "tok-123") || !strings.HasPrefix(raw, cipherPrefix) {
		t.Fatalf("stored secret not encrypted: %q", raw)
	}

	// GetAll decrypts it back.
	vals, err := s.GetAll(ctx)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if vals[KeyPlexToken] != "tok-123" {
		t.Fatalf("decrypted token = %q, want tok-123", vals[KeyPlexToken])
	}
}

func TestStoreSecretPlaintextWithoutKey(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	s := NewStore(store, "") // no key
	if s.Encrypts() {
		t.Fatal("Encrypts() = true without a passphrase, want false")
	}
	if err := s.Set(ctx, KeyJellyfinToken, "jf-tok"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var raw string
	if err := store.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, KeyJellyfinToken).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if raw != "jf-tok" {
		t.Fatalf("no-key storage = %q, want plaintext jf-tok", raw)
	}
}

func TestGetAllUndecryptableSecretIsEmpty(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	// Write a secret under key A.
	if err := NewStore(store, "key-a").Set(ctx, KeyPlexToken, "secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Read with key B: the secret is unreadable → "" (logged), but GetAll must
	// not fail so the app still boots.
	vals, err := NewStore(store, "key-b").GetAll(ctx)
	if err != nil {
		t.Fatalf("GetAll with wrong key: %v", err)
	}
	if vals[KeyPlexToken] != "" {
		t.Fatalf("undecryptable secret = %q, want empty", vals[KeyPlexToken])
	}
}

func TestEncryptPlaintextSecretsOnKeyAdd(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	// Write a token with NO key configured → stored in plaintext.
	if err := NewStore(store, "").Set(ctx, KeyPlexToken, "leaky-token"); err != nil {
		t.Fatalf("Set unkeyed: %v", err)
	}
	var raw string
	store.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, KeyPlexToken).Scan(&raw)
	if raw != "leaky-token" {
		t.Fatalf("precondition: expected plaintext, got %q", raw)
	}

	// Operator later configures a key and the service re-encrypts on boot.
	keyed := NewStore(store, "now-with-key")
	if err := keyed.EncryptPlaintextSecrets(ctx); err != nil {
		t.Fatalf("EncryptPlaintextSecrets: %v", err)
	}

	store.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, KeyPlexToken).Scan(&raw)
	if strings.Contains(raw, "leaky-token") || !strings.HasPrefix(raw, cipherPrefix) {
		t.Fatalf("token not re-encrypted at rest: %q", raw)
	}
	// And it still reads back correctly under the key.
	vals, _ := keyed.GetAll(ctx)
	if vals[KeyPlexToken] != "leaky-token" {
		t.Fatalf("re-encrypted token = %q, want leaky-token", vals[KeyPlexToken])
	}
}

func TestSecretStateTriState(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	keyed := NewStore(store, "key-a")
	if err := keyed.Set(ctx, KeyPlexToken, "tok"); err != nil { // set
		t.Fatalf("Set: %v", err)
	}
	if err := keyed.Set(ctx, KeyJellyfinToken, ""); err != nil { // cleared/unset
		t.Fatalf("Set empty: %v", err)
	}

	st, err := keyed.SecretState(ctx)
	if err != nil {
		t.Fatalf("SecretState: %v", err)
	}
	if st[KeyPlexToken] != "set" {
		t.Errorf("plex state = %q, want set", st[KeyPlexToken])
	}
	if st[KeyJellyfinToken] != "unset" {
		t.Errorf("jellyfin state = %q, want unset", st[KeyJellyfinToken])
	}

	// A different key can't decrypt the plex token → unreadable.
	st2, _ := NewStore(store, "key-b").SecretState(ctx)
	if st2[KeyPlexToken] != "unreadable" {
		t.Errorf("plex state under wrong key = %q, want unreadable", st2[KeyPlexToken])
	}
}

func TestEffectiveBufferCeiling(t *testing.T) {
	if _, err := Effective(&config.Config{}, map[string]string{KeyDownloadBuffer: "1G"}); err == nil {
		t.Fatal("Effective with 1G buffer (> ceiling): want error, got nil")
	}
	if _, err := Effective(&config.Config{}, map[string]string{KeyDownloadBuffer: "4M"}); err != nil {
		t.Fatalf("Effective with 4M buffer: unexpected error %v", err)
	}
}

func TestStoreDelete(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	s := NewStore(store, "")
	if err := s.Set(ctx, KeyPlexURL, "http://plex"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete(ctx, KeyPlexURL); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	vals, _ := s.GetAll(ctx)
	if _, ok := vals[KeyPlexURL]; ok {
		t.Fatalf("key still present after Delete: %v", vals)
	}
}

func TestEffectiveOverlay(t *testing.T) {
	base := &config.Config{
		PlexURL:             "http://env-plex",
		PlexToken:           "env-tok",
		DownloadConcurrency: 2,
		DownloadBufferBytes: 1 << 20,
	}
	vals := map[string]string{
		KeyPlexURL:             "http://db-plex", // override
		KeyJellyfinURL:         "http://db-jelly",
		KeyStorageHardCap:      "10G",
		KeyStorageSoftCap:      "8G",
		KeyDownloadConcurrency: "4",
		KeyDownloadPollEvery:   "30s",
	}
	eff, err := Effective(base, vals)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if eff.PlexURL != "http://db-plex" {
		t.Errorf("PlexURL = %q, want db override", eff.PlexURL)
	}
	if eff.PlexToken != "env-tok" {
		t.Errorf("PlexToken = %q, want env value preserved (not in vals)", eff.PlexToken)
	}
	if eff.JellyfinURL != "http://db-jelly" {
		t.Errorf("JellyfinURL = %q", eff.JellyfinURL)
	}
	if eff.StorageHardCapBytes != 10<<30 || eff.StorageSoftCapBytes != 8<<30 {
		t.Errorf("caps = %d/%d, want 10G/8G", eff.StorageHardCapBytes, eff.StorageSoftCapBytes)
	}
	if eff.DownloadConcurrency != 4 {
		t.Errorf("concurrency = %d, want 4", eff.DownloadConcurrency)
	}
	if eff.DownloadPollEvery.String() != "30s" {
		t.Errorf("poll = %s, want 30s", eff.DownloadPollEvery)
	}
	// base must be untouched (shallow copy semantics).
	if base.PlexURL != "http://env-plex" {
		t.Errorf("base mutated: PlexURL = %q", base.PlexURL)
	}
}

func TestEffectiveEmptyValueClearsString(t *testing.T) {
	base := &config.Config{PlexURL: "http://env-plex", PlexToken: "env-tok"}
	eff, err := Effective(base, map[string]string{KeyPlexURL: "", KeyPlexToken: ""})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if eff.PlexURL != "" || eff.PlexToken != "" {
		t.Fatalf("present-but-empty should clear: url=%q tok=%q", eff.PlexURL, eff.PlexToken)
	}
}

func TestEffectiveEmptyNumberKeepsBase(t *testing.T) {
	base := &config.Config{DownloadConcurrency: 3}
	eff, err := Effective(base, map[string]string{KeyDownloadConcurrency: ""})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if eff.DownloadConcurrency != 3 {
		t.Fatalf("empty concurrency = %d, want base 3 kept", eff.DownloadConcurrency)
	}
}

func TestEffectiveLibraryDirs(t *testing.T) {
	base := &config.Config{MoviesDir: "movies", ShowsDir: "shows", OtherDir: "other"}
	eff, err := Effective(base, map[string]string{
		KeyLibraryShowsDir:  "tv", // override
		KeyLibraryMoviesDir: "",   // present-but-empty keeps the base default
	})
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if eff.ShowsDir != "tv" {
		t.Errorf("ShowsDir = %q, want tv", eff.ShowsDir)
	}
	if eff.MoviesDir != "movies" {
		t.Errorf("MoviesDir = %q, want base movies kept on empty", eff.MoviesDir)
	}
	if eff.OtherDir != "other" {
		t.Errorf("OtherDir = %q, want base other (not in vals)", eff.OtherDir)
	}
}

func TestEffectiveValidation(t *testing.T) {
	base := &config.Config{}
	cases := []struct {
		name string
		vals map[string]string
	}{
		{"soft>hard", map[string]string{KeyStorageHardCap: "5G", KeyStorageSoftCap: "9G"}},
		{"bad concurrency", map[string]string{KeyDownloadConcurrency: "99"}},
		{"bad size", map[string]string{KeyStorageHardCap: "lots"}},
		{"bad duration", map[string]string{KeyDownloadPollEvery: "soon"}},
		{"bad library dir", map[string]string{KeyLibraryShowsDir: "tv/series"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Effective(base, c.vals); err == nil {
				t.Fatalf("Effective(%v): want error, got nil", c.vals)
			}
		})
	}
}
