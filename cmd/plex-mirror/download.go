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
	"github.com/maxdubrinsky/plex-mirror/internal/download"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/source/jellyfin"
	"github.com/maxdubrinsky/plex-mirror/internal/source/plex"
	"github.com/maxdubrinsky/plex-mirror/internal/storage"
)

// cmdDownload implements `plex-mirror download --source=plex --item=ID`.
// One-shot: it queues (idempotent) and runs the download synchronously. Safe
// to invoke a second time to resume after a kill — the engine reconciles the
// partial file and continues from the right offset.
func cmdDownload(args []string) int {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	srcName := fs.String("source", "plex", "source to pull from (only 'plex' resolves URLs today)")
	itemID := fs.String("item", "", "remote item id to download (Plex ratingKey)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *itemID == "" {
		fmt.Fprintln(os.Stderr, "download: --item is required")
		return 2
	}
	if *srcName != "plex" {
		fmt.Fprintf(os.Stderr, "download: unsupported --source %q (only 'plex' has ResolveDownloadURL)\n", *srcName)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	if cfg.PlexURL == "" || cfg.PlexToken == "" {
		fmt.Fprintln(os.Stderr, "download: set PLEXMIRROR_PLEX_URL and PLEXMIRROR_PLEX_TOKEN")
		return 1
	}

	plexSrc, err := plex.New(plex.Config{BaseURL: cfg.PlexURL, Token: cfg.PlexToken})
	if err != nil {
		fmt.Fprintf(os.Stderr, "download: %v\n", err)
		return 1
	}
	resolver, ok := plexSrc.(source.DownloadResolver)
	if !ok {
		fmt.Fprintln(os.Stderr, "download: plex client does not implement DownloadResolver")
		return 1
	}

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db open: %v\n", err)
		return 1
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "db migrate: %v\n", err)
		return 1
	}

	storageMgr := storage.NewManager(store, storage.Policy{
		MediaRoot:    cfg.MediaRoot,
		HardCapBytes: cfg.StorageHardCapBytes,
		SoftCapBytes: cfg.StorageSoftCapBytes,
	})

	var scanner download.Scanner
	if cfg.JellyfinURL != "" && cfg.JellyfinToken != "" {
		sc, err := jellyfin.NewScanner(jellyfin.Config{
			BaseURL: cfg.JellyfinURL, Token: cfg.JellyfinToken,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "download: jellyfin scanner: %v\n", err)
			return 1
		}
		scanner = sc
	}

	engine, err := download.New(store, storageMgr, download.Options{
		MediaRoot:   cfg.MediaRoot,
		MoviesDir:   cfg.MoviesDir,
		ShowsDir:    cfg.ShowsDir,
		OtherDir:    cfg.OtherDir,
		Concurrency: cfg.DownloadConcurrency,
		Scanner:     scanner,
		SourceName:  "plex",
		Resolver:    resolver,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "download: %v\n", err)
		return 1
	}

	// Fetch metadata so the engine has title/container/size_bytes to validate
	// the layout and verify integrity. ResolveDownloadURL alone doesn't carry
	// those; the items table needs them.
	metaCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	item, err := plexSrc.GetMetadata(metaCtx, *itemID)
	cancel()
	if err != nil {
		return reportSourceErr(os.Stderr, err)
	}

	id, err := engine.Queue(ctx, item)
	if err != nil {
		fmt.Fprintf(os.Stderr, "download: queue: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "queued id=%d source_key=%s title=%q\n", id, item.ID, item.Title)

	if err := engine.Download(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "download: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"id":         id,
		"source":     "plex",
		"source_key": item.ID,
		"title":      item.Title,
		"status":     "ready",
	})
	return 0
}
