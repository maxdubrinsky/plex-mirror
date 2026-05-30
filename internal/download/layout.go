package download

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

// Layout builds the Jellyfin-friendly path for an item under MediaRoot. The
// per-kind subdirectory names are configurable (MoviesDir/ShowsDir/OtherDir);
// the defaults below match Jellyfin's common layout:
//
//	Movies   -> <root>/movies/<title> (<year>).<ext>
//	Episodes -> <root>/shows/<show>/Season NN/<show> - sNNeNN - <title>.<ext>
//	Other    -> <root>/other/<title>.<ext>
//
// The partial path used during download is <root>/.partials/<hash>.tmp where
// hash = sha256(source ":" source_key)[:16]; the dot prefix keeps Jellyfin from
// indexing the in-flight file.
type Layout struct {
	MediaRoot string

	// MoviesDir, ShowsDir and OtherDir name the per-kind subdirectories under
	// MediaRoot. Empty falls back to the historical defaults (movies/shows/other)
	// so a bare Layout{MediaRoot: …} — e.g. the cancel path, which only calls
	// Partial — keeps working without spelling them out.
	MoviesDir string
	ShowsDir  string
	OtherDir  string
}

// dirOr returns the configured directory name, or def when it's empty.
func dirOr(name, def string) string {
	if name == "" {
		return def
	}
	return name
}

// Final returns the absolute destination path for item once the download
// completes. Source identifies which adapter produced the item (e.g. "plex")
// and is included in the partial hash to keep namespaces separate when more
// than one source is wired up.
func (l Layout) Final(src string, item source.Item) (string, error) {
	if l.MediaRoot == "" {
		return "", fmt.Errorf("layout: MediaRoot is required")
	}
	ext := strings.TrimPrefix(strings.ToLower(item.Container), ".")
	if ext == "" {
		return "", fmt.Errorf("layout: item %q has no container/extension", item.ID)
	}

	switch item.Kind {
	case source.ItemMovie:
		title := sanitize(item.Title)
		if title == "" {
			return "", fmt.Errorf("layout: movie %q has no title", item.ID)
		}
		name := title
		if item.Year > 0 {
			name = fmt.Sprintf("%s (%d)", title, item.Year)
		}
		return filepath.Join(l.MediaRoot, dirOr(l.MoviesDir, "movies"), name+"."+ext), nil

	case source.ItemEpisode:
		show := sanitize(item.ShowTitle)
		if show == "" {
			return "", fmt.Errorf("layout: episode %q missing show title", item.ID)
		}
		if item.SeasonNumber < 0 || item.EpisodeNumber < 0 {
			return "", fmt.Errorf("layout: episode %q has negative season/episode number", item.ID)
		}
		title := sanitize(item.Title)
		if title == "" {
			title = fmt.Sprintf("Episode %d", item.EpisodeNumber)
		}
		seasonDir := fmt.Sprintf("Season %02d", item.SeasonNumber)
		base := fmt.Sprintf("%s - s%02de%02d - %s.%s",
			show, item.SeasonNumber, item.EpisodeNumber, title, ext)
		return filepath.Join(l.MediaRoot, dirOr(l.ShowsDir, "shows"), show, seasonDir, base), nil

	default:
		title := sanitize(item.Title)
		if title == "" {
			return "", fmt.Errorf("layout: item %q has no title", item.ID)
		}
		return filepath.Join(l.MediaRoot, dirOr(l.OtherDir, "other"), title+"."+ext), nil
	}
}

// Partial returns the absolute temp path used while bytes are still being
// written. Deterministic on (src, sourceKey) so the engine can resume after a
// restart without writing the path into the DB.
func (l Layout) Partial(src, sourceKey string) string {
	sum := sha256.Sum256([]byte(src + ":" + sourceKey))
	name := hex.EncodeToString(sum[:8]) + ".tmp"
	return filepath.Join(l.MediaRoot, ".partials", name)
}

// sanitize strips characters that confuse common filesystems (and Jellyfin's
// parser) while keeping the result human-readable. Unicode letters/digits pass
// through; control chars and the Windows-reserved set become "-"; the result
// is trimmed of leading/trailing dots and spaces and capped at 200 chars so
// the full path stays comfortably under typical 255-byte component limits.
func sanitize(s string) string {
	// Render "Title: Subtitle" as "Title - Subtitle" rather than letting the
	// bare-colon rule below turn it into "Title- Subtitle". A colon NOT followed
	// by a space (e.g. "11:14") still falls through to the forbidden-set "-".
	s = strings.ReplaceAll(s, ": ", " - ")

	const forbidden = `/\:*?"<>|`
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			// collapse runs of whitespace (incl. tab/newline) to one space
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		case r < 0x20 || r == 0x7f:
			// drop remaining control chars entirely
			continue
		case strings.ContainsRune(forbidden, r):
			b.WriteRune('-')
			prevSpace = false
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	out := strings.TrimFunc(b.String(), func(r rune) bool {
		return r == '.' || unicode.IsSpace(r)
	})
	if len(out) > 200 {
		out = strings.TrimRight(out[:200], " .")
	}
	return out
}
