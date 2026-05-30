package download

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxdubrinsky/plex-mirror/internal/source"
)

func TestLayoutFinalMovie(t *testing.T) {
	l := Layout{MediaRoot: "/media"}
	got, err := l.Final("plex", source.Item{
		ID: "1", Title: "Blade Runner", Kind: source.ItemMovie, Year: 1982, Container: "mkv",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/media/movies/Blade Runner (1982).mkv")
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestLayoutFinalMovieNoYear(t *testing.T) {
	l := Layout{MediaRoot: "/media"}
	got, err := l.Final("plex", source.Item{
		ID: "1", Title: "Unreleased", Kind: source.ItemMovie, Container: "mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "/media/movies/Unreleased.mp4"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestLayoutFinalEpisode(t *testing.T) {
	l := Layout{MediaRoot: "/media"}
	got, err := l.Final("plex", source.Item{
		ID:            "9",
		Title:         "Pilot",
		Kind:          source.ItemEpisode,
		ShowTitle:     "Severance",
		SeasonNumber:  1,
		EpisodeNumber: 1,
		Container:     "mkv",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "/media/shows/Severance/Season 01/Severance - s01e01 - Pilot.mkv"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestLayoutFinalCustomDirs(t *testing.T) {
	l := Layout{MediaRoot: "/media", MoviesDir: "films", ShowsDir: "tv", OtherDir: "misc"}

	movie, err := l.Final("plex", source.Item{
		ID: "1", Title: "Blade Runner", Kind: source.ItemMovie, Year: 1982, Container: "mkv",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/media/films/Blade Runner (1982).mkv"; movie != want {
		t.Errorf("movie: got %q want %q", movie, want)
	}

	ep, err := l.Final("plex", source.Item{
		ID: "9", Title: "Pilot", Kind: source.ItemEpisode,
		ShowTitle: "Severance", SeasonNumber: 1, EpisodeNumber: 1, Container: "mkv",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/media/tv/Severance/Season 01/Severance - s01e01 - Pilot.mkv"; ep != want {
		t.Errorf("episode: got %q want %q", ep, want)
	}
}

func TestLayoutFinalMissingContainer(t *testing.T) {
	l := Layout{MediaRoot: "/media"}
	_, err := l.Final("plex", source.Item{
		ID: "1", Title: "X", Kind: source.ItemMovie,
	})
	if err == nil {
		t.Fatal("expected error for missing container")
	}
}

func TestLayoutFinalMissingShow(t *testing.T) {
	l := Layout{MediaRoot: "/media"}
	_, err := l.Final("plex", source.Item{
		ID: "9", Title: "Pilot", Kind: source.ItemEpisode, Container: "mkv",
	})
	if err == nil {
		t.Fatal("expected error for missing show title")
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"hello/world":         "hello-world",
		"a:b*c?d":             "a-b-c-d", // bare colon (no following space) -> dash
		"  spaced  out  ":     "spaced out",
		".hidden.":            "hidden",
		"with\x00null":        "withnull",
		"tabs\tand\nbreak":    "tabs and break",
		"Mission: Impossible": "Mission - Impossible", // ": " -> " - "
		"Bo Burnham: what.":   "Bo Burnham - what",
		"11:14":               "11-14", // colon without space stays a dash
	}
	for in, want := range cases {
		got := sanitize(in)
		if got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLayoutPartialDeterministic(t *testing.T) {
	l := Layout{MediaRoot: "/media"}
	a := l.Partial("plex", "42")
	b := l.Partial("plex", "42")
	if a != b {
		t.Fatalf("partial paths should be deterministic, got %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "/media/.partials/") || !strings.HasSuffix(a, ".tmp") {
		t.Errorf("unexpected partial path: %q", a)
	}
	c := l.Partial("plex", "43")
	if a == c {
		t.Errorf("partial path collided across keys: %q", a)
	}
}
