package config

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// MaxDownloadBuffer caps the per-stream copy buffer. A buffer this size is far
// past the point of diminishing returns for a single sequential transfer, and
// the ceiling keeps an absurd value from wrapping the int conversion in the
// engine or attempting a giant per-stream allocation.
const MaxDownloadBuffer = 256 << 20 // 256 MiB

type Config struct {
	HTTPAddr  string
	DBPath    string
	MediaRoot string
	LogLevel  string

	// AuthToken gates the /mcp endpoint (and, later, the portal) with a static
	// bearer token. Empty means no app-level auth — intended for deployments
	// that put Traefik forward-auth in front. /healthz is always open.
	AuthToken string

	// Source credentials. All optional at boot — the server runs without
	// them; the CLI dump/sync subcommands surface a friendly error when
	// the relevant pair is missing.
	//
	// Plex can be located two ways:
	//   - PlexURL set      → talk to that server directly; PlexToken is whatever
	//                        token works for it (a per-resource token for a share).
	//   - PlexURL empty +  → discover the server's connection + per-resource token
	//     PlexServer set     from plex.tv using PlexToken as the ACCOUNT token.
	// PlexURL wins when both are set.
	PlexURL      string
	PlexToken    string
	PlexServer   string // server name or clientIdentifier to discover via plex.tv
	PlexClientID string // X-Plex-Client-Identifier for discovery; blank = adapter default

	JellyfinURL   string
	JellyfinToken string
	JellyfinUser  string // user id or username; required only when the token is a server API key and >1 user exists

	// Storage manager knobs. Zero values disable that behavior.
	StorageHardCapBytes int64
	StorageSoftCapBytes int64
	StorageSweepEvery   time.Duration

	// Download engine knobs.
	DownloadConcurrency int           // simultaneous downloads, default 2
	DownloadPollEvery   time.Duration // daemon poll interval for queued items, 0 = off in CLI mode
	DownloadBufferBytes int64         // copy buffer per stream, default 1 MiB (see glb-gdl.14)

	// SecretKey is the passphrase used to encrypt DB-stored secrets (source
	// tokens set via the portal settings page). Empty disables encryption and
	// secrets are stored in plaintext in the local SQLite file. Env-only — never
	// editable from the portal. See glb-gdl.13.
	SecretKey string
}

func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:      envDefault("PLEXMIRROR_HTTP_ADDR", ":8080"),
		DBPath:        envDefault("PLEXMIRROR_DB_PATH", "/var/lib/plex-mirror/state.db"),
		MediaRoot:     envDefault("PLEXMIRROR_MEDIA_ROOT", "/media"),
		LogLevel:      envDefault("PLEXMIRROR_LOG_LEVEL", "info"),
		PlexURL:       os.Getenv("PLEXMIRROR_PLEX_URL"),
		PlexToken:     os.Getenv("PLEXMIRROR_PLEX_TOKEN"),
		PlexServer:    os.Getenv("PLEXMIRROR_PLEX_SERVER"),
		PlexClientID:  os.Getenv("PLEXMIRROR_PLEX_CLIENT_ID"),
		JellyfinURL:   os.Getenv("PLEXMIRROR_JELLYFIN_URL"),
		JellyfinToken: os.Getenv("PLEXMIRROR_JELLYFIN_TOKEN"),
		JellyfinUser:  os.Getenv("PLEXMIRROR_JELLYFIN_USER"),
		AuthToken:     os.Getenv("PLEXMIRROR_AUTH_TOKEN"),
		SecretKey:     os.Getenv("PLEXMIRROR_SECRET_KEY"),
	}

	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("invalid PLEXMIRROR_LOG_LEVEL %q (want debug|info|warn|error)", c.LogLevel)
	}

	hard, err := ParseSize(envDefault("PLEXMIRROR_STORAGE_HARD_CAP", "0"))
	if err != nil {
		return nil, fmt.Errorf("PLEXMIRROR_STORAGE_HARD_CAP: %w", err)
	}
	soft, err := ParseSize(envDefault("PLEXMIRROR_STORAGE_SOFT_CAP", "0"))
	if err != nil {
		return nil, fmt.Errorf("PLEXMIRROR_STORAGE_SOFT_CAP: %w", err)
	}
	if hard > 0 && soft > 0 && soft > hard {
		return nil, fmt.Errorf("PLEXMIRROR_STORAGE_SOFT_CAP (%d) must be <= PLEXMIRROR_STORAGE_HARD_CAP (%d)", soft, hard)
	}
	c.StorageHardCapBytes = hard
	c.StorageSoftCapBytes = soft

	sweep, err := time.ParseDuration(envDefault("PLEXMIRROR_STORAGE_SWEEP_EVERY", "5m"))
	if err != nil {
		return nil, fmt.Errorf("PLEXMIRROR_STORAGE_SWEEP_EVERY: %w", err)
	}
	if sweep < 0 {
		return nil, fmt.Errorf("PLEXMIRROR_STORAGE_SWEEP_EVERY: must be >= 0, got %q", sweep)
	}
	c.StorageSweepEvery = sweep

	concRaw := envDefault("PLEXMIRROR_DOWNLOAD_CONCURRENCY", "2")
	conc, err := strconv.Atoi(concRaw)
	if err != nil || conc < 1 || conc > 32 {
		return nil, fmt.Errorf("PLEXMIRROR_DOWNLOAD_CONCURRENCY: want integer 1..32, got %q", concRaw)
	}
	c.DownloadConcurrency = conc

	pollRaw := envDefault("PLEXMIRROR_DOWNLOAD_POLL_EVERY", "10s")
	poll, err := time.ParseDuration(pollRaw)
	if err != nil {
		return nil, fmt.Errorf("PLEXMIRROR_DOWNLOAD_POLL_EVERY: %w", err)
	}
	if poll < 0 {
		return nil, fmt.Errorf("PLEXMIRROR_DOWNLOAD_POLL_EVERY: must be >= 0, got %q", poll)
	}
	c.DownloadPollEvery = poll

	buf, err := ParseSize(envDefault("PLEXMIRROR_DOWNLOAD_BUFFER", "1M"))
	if err != nil {
		return nil, fmt.Errorf("PLEXMIRROR_DOWNLOAD_BUFFER: %w", err)
	}
	if buf > MaxDownloadBuffer {
		return nil, fmt.Errorf("PLEXMIRROR_DOWNLOAD_BUFFER: %d exceeds max %d", buf, MaxDownloadBuffer)
	}
	c.DownloadBufferBytes = buf

	return c, nil
}

// ParseSize accepts a plain integer byte count or one of the common SI/binary
// suffixes: K, M, G, T (1024-based) or KB, MB, GB, TB (still 1024-based — the
// 1000-based variants don't matter for disk quotas in a homelab). Case-insensitive.
// Exported so the portal settings page (glb-gdl.13) validates caps the same way
// the env loader does.
func ParseSize(raw string) (int64, error) {
	s := strings.TrimSpace(strings.ToUpper(raw))
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "TB"):
		mult = 1 << 40
		s = strings.TrimSuffix(s, "TB")
	case strings.HasSuffix(s, "GB"):
		mult = 1 << 30
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult = 1 << 20
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult = 1 << 10
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "T"):
		mult = 1 << 40
		s = strings.TrimSuffix(s, "T")
	case strings.HasSuffix(s, "G"):
		mult = 1 << 30
		s = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult = 1 << 20
		s = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		mult = 1 << 10
		s = strings.TrimSuffix(s, "K")
	}
	s = strings.TrimSpace(s)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a size: %q", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative size: %q", raw)
	}
	// Guard the multiply: n*mult can overflow int64 and wrap to a negative or
	// small value, which would silently bypass the soft<=hard cap check and
	// disable capping. Reject the absurd input instead.
	if mult > 1 && n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size too large: %q", raw)
	}
	return n * mult, nil
}

func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
