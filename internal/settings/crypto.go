package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	// cipherPrefix marks a stored value as AES-GCM ciphertext. A value without it
	// is plaintext (written when no key is configured), so the two states are
	// unambiguous on read even if the operator adds a key later.
	cipherPrefix = "gcm:"

	// saltLen is the per-store KDF salt size; the salt is generated once and kept
	// in the settings table (salts aren't secret — they defeat precomputation and
	// cross-deployment key reuse; the scrypt work factor defeats guessing).
	saltLen = 16

	// scrypt work factors: the standard "interactive" tuning (N=2^15, r=8, p=1,
	// ~tens of ms per derivation). Raises the offline brute-force cost of a
	// human-chosen PLEXMIRROR_SECRET_KEY by orders of magnitude over a bare hash.
	scryptN = 1 << 15
	scryptR = 8
	scryptP = 1
)

// deriveKey stretches the passphrase into a 32-byte AES-256 key using scrypt and
// the given per-store salt. Empty passphrase → nil key → encryption disabled
// (plaintext storage). scrypt (not a bare hash) so an exfiltrated DB can't be
// brute-forced cheaply.
func deriveKey(passphrase string, salt []byte) []byte {
	if passphrase == "" {
		return nil
	}
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		// Unreachable with the constant params above; degrade rather than panic.
		sum := sha256.Sum256(append([]byte(passphrase), salt...))
		return sum[:]
	}
	return key
}

// encryptValue encrypts plaintext with key (AES-256-GCM, random nonce) and
// returns "gcm:<base64(nonce||ciphertext)>". With a nil key it returns the
// plaintext unchanged so callers don't need to special-case the no-key mode.
func encryptValue(key []byte, plaintext string) (string, error) {
	if len(key) == 0 {
		return plaintext, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("settings: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("settings: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("settings: nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return cipherPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptValue reverses encryptValue. A value without the gcm: prefix is
// returned as-is (plaintext). A gcm: value with no/ wrong key is an error so a
// secret never silently reads back as garbage or empty.
func decryptValue(key []byte, stored string) (string, error) {
	if !strings.HasPrefix(stored, cipherPrefix) {
		return stored, nil
	}
	if len(key) == 0 {
		return "", errors.New("settings: encrypted value present but PLEXMIRROR_SECRET_KEY is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, cipherPrefix))
	if err != nil {
		return "", fmt.Errorf("settings: decode ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("settings: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("settings: gcm: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("settings: ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	out, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("settings: decrypt (wrong PLEXMIRROR_SECRET_KEY?): %w", err)
	}
	return string(out), nil
}
