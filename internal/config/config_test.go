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
		{"12X", 0, true},            // unknown suffix leaves "12X" which fails to parse
		{"9999999999T", 0, true},    // n*mult overflows int64 → rejected, not wrapped
		{"8388608T", 0, true},       // wraps to a negative cap if unguarded
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
