package config

import "testing"

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1024", 1024, false},
		{"1K", 1 << 10, false},
		{"1KB", 1 << 10, false},
		{"10M", 10 << 20, false},
		{"2g", 2 << 30, false},
		{"1T", 1 << 40, false},
		{"1MB", 1 << 20, false},
		{"-5", 0, true},
		{"banana", 0, true},
		{"12X", 0, true},         // unknown suffix leaves "12X" which fails to parse
		{"9999999999T", 0, true}, // n*mult overflows int64 → rejected, not wrapped
		{"8388608T", 0, true},    // wraps to a negative cap if unguarded
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestLoadDownloadBufferDefault(t *testing.T) {
	t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DownloadBufferBytes != 1<<20 {
		t.Errorf("DownloadBufferBytes = %d, want %d (1 MiB default)", cfg.DownloadBufferBytes, 1<<20)
	}
}

func TestLoadDownloadBufferOverride(t *testing.T) {
	t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
	t.Setenv("PLEXMIRROR_DOWNLOAD_BUFFER", "4M")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DownloadBufferBytes != 4<<20 {
		t.Errorf("DownloadBufferBytes = %d, want %d", cfg.DownloadBufferBytes, 4<<20)
	}
}

func TestLoadDownloadBufferInvalid(t *testing.T) {
	t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
	t.Setenv("PLEXMIRROR_DOWNLOAD_BUFFER", "notasize")
	if _, err := Load(); err == nil {
		t.Fatal("Load with bad PLEXMIRROR_DOWNLOAD_BUFFER: want error, got nil")
	}
}

func TestLoadDownloadBufferAboveCeiling(t *testing.T) {
	t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
	t.Setenv("PLEXMIRROR_DOWNLOAD_BUFFER", "1G") // > 256 MiB ceiling
	if _, err := Load(); err == nil {
		t.Fatal("Load with oversized buffer: want error, got nil")
	}
}

func TestValidateLibraryDir(t *testing.T) {
	for _, n := range []string{"movies", "tv", "TV Shows", "anime_series"} {
		if err := ValidateLibraryDir(n); err != nil {
			t.Errorf("ValidateLibraryDir(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range []string{"", "a/b", `a\b`, ".", "..", ".partials", ".hidden"} {
		if err := ValidateLibraryDir(n); err == nil {
			t.Errorf("ValidateLibraryDir(%q) = nil, want error", n)
		}
	}
}

func TestLoadLibraryDirsDefault(t *testing.T) {
	t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MoviesDir != "movies" || cfg.ShowsDir != "shows" || cfg.OtherDir != "other" {
		t.Errorf("default dirs = %q/%q/%q, want movies/shows/other",
			cfg.MoviesDir, cfg.ShowsDir, cfg.OtherDir)
	}
}

func TestLoadLibraryDirsOverride(t *testing.T) {
	t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
	t.Setenv("PLEXMIRROR_SHOWS_DIR", "tv")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ShowsDir != "tv" {
		t.Errorf("ShowsDir = %q, want tv", cfg.ShowsDir)
	}
}

func TestLoadLibraryDirsInvalid(t *testing.T) {
	t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
	t.Setenv("PLEXMIRROR_SHOWS_DIR", "tv/series")
	if _, err := Load(); err == nil {
		t.Fatal("Load with PLEXMIRROR_SHOWS_DIR=tv/series: want error, got nil")
	}
}

func TestLoadRejectsNegativeDurations(t *testing.T) {
	for _, key := range []string{"PLEXMIRROR_DOWNLOAD_POLL_EVERY", "PLEXMIRROR_STORAGE_SWEEP_EVERY"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("PLEXMIRROR_MEDIA_ROOT", t.TempDir())
			t.Setenv(key, "-10s")
			if _, err := Load(); err == nil {
				t.Fatalf("Load with %s=-10s: want error, got nil", key)
			}
		})
	}
}
