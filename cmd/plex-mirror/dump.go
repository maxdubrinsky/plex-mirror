package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/source"
	"github.com/maxdubrinsky/plex-mirror/internal/source/jellyfin"
	"github.com/maxdubrinsky/plex-mirror/internal/source/plex"
)

// cmdDump implements `plex-mirror dump --source=plex|jellyfin [--library=ID]`.
// With no --library, it lists libraries. With --library, it lists items.
// Output is JSON on stdout so it's pipe-friendly; errors go to stderr.
func cmdDump(args []string) int {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	srcName := fs.String("source", "", "source to browse: plex|jellyfin")
	libraryID := fs.String("library", "", "library ID to list items from (omit to list libraries)")
	limit := fs.Int("limit", 50, "max items when listing items")
	offset := fs.Int("offset", 0, "offset when listing items")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}

	src, err := buildSource(*srcName, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dump: %v\n", err)
		return 1
	}

	ctx := context.Background()
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")

	if *libraryID == "" {
		libs, err := src.ListLibraries(ctx)
		if err != nil {
			return reportSourceErr(os.Stderr, err)
		}
		_ = out.Encode(map[string]any{"source": src.Name(), "libraries": libs})
		return 0
	}

	items, err := src.ListItems(ctx, *libraryID, source.ListOptions{Offset: *offset, Limit: *limit})
	if err != nil {
		return reportSourceErr(os.Stderr, err)
	}
	_ = out.Encode(map[string]any{"source": src.Name(), "library": *libraryID, "items": items})
	return 0
}

func buildSource(name string, cfg *config.Config) (source.Source, error) {
	switch name {
	case "plex":
		if cfg.PlexURL == "" || cfg.PlexToken == "" {
			return nil, fmt.Errorf("set PLEXMIRROR_PLEX_URL and PLEXMIRROR_PLEX_TOKEN before --source=plex")
		}
		return plex.New(plex.Config{BaseURL: cfg.PlexURL, Token: cfg.PlexToken})
	case "jellyfin":
		if cfg.JellyfinURL == "" || cfg.JellyfinToken == "" {
			return nil, fmt.Errorf("set PLEXMIRROR_JELLYFIN_URL and PLEXMIRROR_JELLYFIN_TOKEN before --source=jellyfin")
		}
		return jellyfin.New(jellyfin.Config{BaseURL: cfg.JellyfinURL, Token: cfg.JellyfinToken})
	case "":
		return nil, fmt.Errorf("missing --source (want plex or jellyfin)")
	default:
		return nil, fmt.Errorf("unknown --source %q (want plex or jellyfin)", name)
	}
}

func reportSourceErr(w io.Writer, err error) int {
	switch {
	case errors.Is(err, source.ErrAuth):
		fmt.Fprintf(w, "dump: auth failed: %v\n", err)
		return 3
	case errors.Is(err, source.ErrNotFound):
		fmt.Fprintf(w, "dump: not found: %v\n", err)
		return 4
	case errors.Is(err, source.ErrNetwork):
		fmt.Fprintf(w, "dump: network: %v\n", err)
		return 5
	default:
		fmt.Fprintf(w, "dump: %v\n", err)
		return 1
	}
}
