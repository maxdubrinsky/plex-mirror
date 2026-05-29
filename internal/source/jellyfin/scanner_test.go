package jellyfin

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNewScanner_ValidatesConfig(t *testing.T) {
	if _, err := NewScanner(Config{Token: "t"}); err == nil {
		t.Error("expected error for missing BaseURL")
	}
	if _, err := NewScanner(Config{BaseURL: "http://localhost:8096"}); err == nil {
		t.Error("expected error for missing Token")
	}
}

// TestScannerRefreshLive hits a real Jellyfin server. It is gated on the same
// env vars the binary uses, so it is a no-op in CI / for anyone without a
// server configured. The token must be an admin API key — /Library/Refresh
// returns 403 for a plain user token.
//
//	PLEXMIRROR_JELLYFIN_URL=http://host:8096 \
//	PLEXMIRROR_JELLYFIN_TOKEN=<admin api key> \
//	go test ./internal/source/jellyfin -run TestScannerRefreshLive -v
func TestScannerRefreshLive(t *testing.T) {
	url := os.Getenv("PLEXMIRROR_JELLYFIN_URL")
	token := os.Getenv("PLEXMIRROR_JELLYFIN_TOKEN")
	if url == "" || token == "" {
		t.Skip("set PLEXMIRROR_JELLYFIN_URL and PLEXMIRROR_JELLYFIN_TOKEN to run")
	}

	sc, err := NewScanner(Config{BaseURL: url, Token: token})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sc.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	t.Log("Refresh accepted by Jellyfin (library scan triggered)")
}
