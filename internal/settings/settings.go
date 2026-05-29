// Package settings is the DB-backed configuration layer (glb-gdl.13). It stores
// portal-editable config in the `settings` table and overlays it on top of the
// env bootstrap (config.Config from PLEXMIRROR_*), so an operator can add/edit a
// Plex/Jellyfin source and storage caps from the web portal without touching the
// environment. Source tokens are encrypted at rest (AES-GCM, see crypto.go) and
// never rendered back to the UI.
//
// Layering: env is the base; a DB row for a key overrides it. The service reads
// Effective(base, GetAll()) at boot and on every live reload.
package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
)

// Setting keys. Dotted names group fields by subsystem. These are the only keys
// the portal writes; MediaRoot, AuthToken, and SecretKey stay env-only (the
// first two would let a misconfig lock the operator out; the third is the key
// that protects the others, so it can't live in the store it protects).
const (
	KeyPlexURL             = "plex.url"
	KeyPlexToken           = "plex.token" // secret
	KeyPlexServer          = "plex.server"
	KeyPlexClientID        = "plex.client_id"
	KeyJellyfinURL         = "jellyfin.url"
	KeyJellyfinToken       = "jellyfin.token" // secret
	KeyJellyfinUser        = "jellyfin.user"
	KeyStorageHardCap      = "storage.hard_cap"
	KeyStorageSoftCap      = "storage.soft_cap"
	KeyDownloadConcurrency = "download.concurrency"
	KeyDownloadPollEvery   = "download.poll_every"
	KeyDownloadBuffer      = "download.buffer"

	// kdfSaltKey holds the per-store scrypt salt. The "$" prefix marks it
	// reserved/internal: GetAll filters it out and it's never portal-editable.
	kdfSaltKey = "$kdf.salt"
)

// secretKeys flags which settings hold credentials: stored encrypted (when a
// PLEXMIRROR_SECRET_KEY is configured) and never returned to the portal as
// plaintext for rendering.
var secretKeys = map[string]bool{
	KeyPlexToken:     true,
	KeyJellyfinToken: true,
}

// IsSecret reports whether a key holds a credential.
func IsSecret(key string) bool { return secretKeys[key] }

// Store reads and writes the settings table, transparently encrypting secret
// values with the configured key. A nil/empty key means plaintext storage (the
// documented no-PLEXMIRROR_SECRET_KEY fallback).
type Store struct {
	db  *db.Store
	key []byte // AES-256 key derived from passphrase+salt via scrypt; nil disables encryption
}

// NewStore builds a settings store. passphrase is PLEXMIRROR_SECRET_KEY; empty
// disables at-rest encryption (values stored as plaintext). When a passphrase is
// set, the key is derived with scrypt against a per-store salt persisted in the
// settings table (generated on first use).
func NewStore(database *db.Store, passphrase string) *Store {
	s := &Store{db: database}
	if passphrase != "" {
		s.key = deriveKey(passphrase, s.loadOrCreateSalt())
	}
	return s
}

// Encrypts reports whether at-rest encryption is active (a key is configured).
func (s *Store) Encrypts() bool { return len(s.key) > 0 }

// loadOrCreateSalt returns the persisted scrypt salt, generating + storing one on
// first use. Salts are not secret (they defeat precomputation and cross-store key
// reuse; the scrypt work factor defeats guessing), so storing it beside the
// ciphertext is fine. On a DB error it logs and returns a freshly-generated salt
// so boot never fails — the degenerate case where that salt isn't persisted only
// arises when the DB itself is unwritable.
func (s *Store) loadOrCreateSalt() []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if salt, ok := s.readSalt(ctx); ok {
		return salt
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		slog.Warn("settings: KDF salt generation failed; using a fixed fallback", "err", err)
		return []byte("plex-mirror-fixed")[:saltLen]
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, is_secret, updated_at)
		VALUES (?, ?, 0, unixepoch())
		ON CONFLICT(key) DO NOTHING
	`, kdfSaltKey, base64.StdEncoding.EncodeToString(salt)); err != nil {
		slog.Warn("settings: persisting KDF salt failed; encrypted secrets may not survive restart", "err", err)
		return salt
	}
	// Re-read: our INSERT may have lost a race (ON CONFLICT DO NOTHING), in which
	// case the already-stored salt is authoritative.
	if stored, ok := s.readSalt(ctx); ok {
		return stored
	}
	return salt
}

func (s *Store) readSalt(ctx context.Context) ([]byte, bool) {
	var b64 string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, kdfSaltKey).Scan(&b64); err != nil {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

// GetAll loads every (non-reserved) stored setting, decrypting secrets. A secret
// that can't be decrypted (key rotated away, or set after plaintext was written
// under no key) is logged and returned as "" so the service still boots
// browse-only rather than failing hard on an unreadable token. Only genuine
// DB/scan failures are returned as errors.
func (s *Store) GetAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, is_secret FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: query: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var key, value string
		var isSecret int
		if err := rows.Scan(&key, &value, &isSecret); err != nil {
			return nil, fmt.Errorf("settings: scan: %w", err)
		}
		if strings.HasPrefix(key, "$") {
			continue // reserved/internal (e.g. the KDF salt) — not config
		}
		if isSecret == 1 || secretKeys[key] {
			plain, derr := decryptValue(s.key, value)
			if derr != nil {
				slog.Warn("settings: could not decrypt stored secret; treating as unset",
					"key", key, "err", derr)
				value = ""
			} else {
				value = plain
			}
		}
		out[key] = value
	}
	return out, rows.Err()
}

// SecretState reports each stored secret key's state for the settings UI:
// "set", "unset" (cleared/empty), or "unreadable" (present but undecryptable
// with the current key — i.e. the key was rotated/misconfigured). Keys with no
// row are absent from the map.
func (s *Store) SecretState(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE is_secret = 1`)
	if err != nil {
		return nil, fmt.Errorf("settings: secret state: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("settings: scan secret: %w", err)
		}
		plain, derr := decryptValue(s.key, value)
		switch {
		case derr != nil:
			out[key] = "unreadable"
		case plain == "":
			out[key] = "unset"
		default:
			out[key] = "set"
		}
	}
	return out, rows.Err()
}

// Set upserts one setting, encrypting it first when the key is a secret.
func (s *Store) Set(ctx context.Context, key, value string) error {
	stored, isSecret, err := s.encodeForStore(key, value)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, is_secret, updated_at)
		VALUES (?, ?, ?, unixepoch())
		ON CONFLICT(key) DO UPDATE SET
			value      = excluded.value,
			is_secret  = excluded.is_secret,
			updated_at = unixepoch()
	`, key, stored, isSecret); err != nil {
		return fmt.Errorf("settings: set %q: %w", key, err)
	}
	return nil
}

// SetMany upserts several settings in a single transaction, encrypting secret
// keys. Either every row is written or none — so a mid-write failure (DB locked,
// disk full) can't leave a half-applied config even though the values were
// validated as a set.
func (s *Store) SetMany(ctx context.Context, kv map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for key, value := range kv {
		stored, isSecret, eerr := s.encodeForStore(key, value)
		if eerr != nil {
			return eerr
		}
		if _, eerr := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, is_secret, updated_at)
			VALUES (?, ?, ?, unixepoch())
			ON CONFLICT(key) DO UPDATE SET
				value      = excluded.value,
				is_secret  = excluded.is_secret,
				updated_at = unixepoch()
		`, key, stored, isSecret); eerr != nil {
			return fmt.Errorf("settings: set %q: %w", key, eerr)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: commit: %w", err)
	}
	return nil
}

// encodeForStore prepares a (value, is_secret) pair for storage, encrypting
// secret keys with the configured key.
func (s *Store) encodeForStore(key, value string) (string, int, error) {
	if !secretKeys[key] {
		return value, 0, nil
	}
	enc, err := encryptValue(s.key, value)
	if err != nil {
		return "", 0, fmt.Errorf("settings: encrypt %q: %w", key, err)
	}
	return enc, 1, nil
}

// EncryptPlaintextSecrets re-encrypts any secret row still stored in plaintext —
// written while no key was configured — now that a key is set. No-op when
// encryption is disabled. Idempotent. Run at boot so adding PLEXMIRROR_SECRET_KEY
// after the fact actually closes the at-rest exposure rather than leaving old
// tokens in cleartext.
func (s *Store) EncryptPlaintextSecrets(ctx context.Context) error {
	if !s.Encrypts() {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE is_secret = 1`)
	if err != nil {
		return fmt.Errorf("settings: scan secrets: %w", err)
	}
	type kv struct{ key, value string }
	var plaintext []kv
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return fmt.Errorf("settings: scan secret: %w", err)
		}
		if v != "" && !strings.HasPrefix(v, cipherPrefix) {
			plaintext = append(plaintext, kv{k, v})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Rewrite after closing rows — the DB runs at MaxOpenConns(1), so we can't
	// issue writes while a read cursor is open.
	for _, p := range plaintext {
		if err := s.Set(ctx, p.key, p.value); err != nil {
			return err
		}
		slog.Info("settings: re-encrypted previously-plaintext secret at rest", "key", p.key)
	}
	return nil
}

// Delete removes a setting, reverting that field to its env value on the next
// Effective() pass.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("settings: delete %q: %w", key, err)
	}
	return nil
}

// Effective overlays the DB-stored settings onto the env bootstrap base and
// returns the merged config. base is shallow-copied and never mutated.
//
// Semantics per field kind:
//   - strings / secrets: a present key sets the field, INCLUDING empty (so the
//     portal can clear an env-provided URL or token).
//   - size caps: present → ParseSize (empty → 0 = uncapped/unset).
//   - concurrency / poll / buffer: present AND non-empty → parse + validate;
//     a present-but-empty value keeps the base (a clean "reset to default").
//
// Validation mirrors config.Load (soft<=hard, concurrency 1..32, buffer ceiling,
// parseable durations/sizes) so a bad stored value can never produce an invalid
// running config — Effective returns an error instead and the caller keeps the
// prior config.
func Effective(base *config.Config, vals map[string]string) (*config.Config, error) {
	eff := *base // shallow copy; pointers/maps inside are not mutated below

	if v, ok := vals[KeyPlexURL]; ok {
		eff.PlexURL = v
	}
	if v, ok := vals[KeyPlexToken]; ok {
		eff.PlexToken = v
	}
	if v, ok := vals[KeyPlexServer]; ok {
		eff.PlexServer = v
	}
	if v, ok := vals[KeyPlexClientID]; ok {
		eff.PlexClientID = v
	}
	if v, ok := vals[KeyJellyfinURL]; ok {
		eff.JellyfinURL = v
	}
	if v, ok := vals[KeyJellyfinToken]; ok {
		eff.JellyfinToken = v
	}
	if v, ok := vals[KeyJellyfinUser]; ok {
		eff.JellyfinUser = v
	}

	if v, ok := vals[KeyStorageHardCap]; ok {
		n, err := config.ParseSize(v)
		if err != nil {
			return nil, fmt.Errorf("settings: %s: %w", KeyStorageHardCap, err)
		}
		eff.StorageHardCapBytes = n
	}
	if v, ok := vals[KeyStorageSoftCap]; ok {
		n, err := config.ParseSize(v)
		if err != nil {
			return nil, fmt.Errorf("settings: %s: %w", KeyStorageSoftCap, err)
		}
		eff.StorageSoftCapBytes = n
	}

	if v, ok := vals[KeyDownloadConcurrency]; ok && strings.TrimSpace(v) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 || n > 32 {
			return nil, fmt.Errorf("settings: %s: want integer 1..32, got %q", KeyDownloadConcurrency, v)
		}
		eff.DownloadConcurrency = n
	}
	if v, ok := vals[KeyDownloadPollEvery]; ok && strings.TrimSpace(v) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("settings: %s: %w", KeyDownloadPollEvery, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("settings: %s: must be >= 0, got %q", KeyDownloadPollEvery, v)
		}
		eff.DownloadPollEvery = d
	}
	if v, ok := vals[KeyDownloadBuffer]; ok && strings.TrimSpace(v) != "" {
		n, err := config.ParseSize(v)
		if err != nil {
			return nil, fmt.Errorf("settings: %s: %w", KeyDownloadBuffer, err)
		}
		if n > config.MaxDownloadBuffer {
			return nil, fmt.Errorf("settings: %s: %d exceeds max %d", KeyDownloadBuffer, n, config.MaxDownloadBuffer)
		}
		eff.DownloadBufferBytes = n
	}

	if eff.StorageHardCapBytes > 0 && eff.StorageSoftCapBytes > 0 &&
		eff.StorageSoftCapBytes > eff.StorageHardCapBytes {
		return nil, fmt.Errorf("settings: soft cap (%d) must be <= hard cap (%d)",
			eff.StorageSoftCapBytes, eff.StorageHardCapBytes)
	}
	return &eff, nil
}
