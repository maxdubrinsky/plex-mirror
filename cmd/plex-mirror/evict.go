package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// cmdEvictNow runs a one-shot eviction pass and reports what was removed.
// Useful as an operator escape hatch when the soft cap creeps past.
func cmdEvictNow(args []string) int {
	fs := flag.NewFlagSet("evict-now", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "report what would be evicted without removing anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db open: %v\n", err)
		return 1
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "db migrate: %v\n", err)
		return 1
	}

	mgr := storage.NewManager(store, storage.Policy{
		MediaRoot:    cfg.MediaRoot,
		HardCapBytes: cfg.StorageHardCapBytes,
		SoftCapBytes: cfg.StorageSoftCapBytes,
	})

	report, err := mgr.EvictNow(ctx, *dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "evict: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report)
	return 0
}
