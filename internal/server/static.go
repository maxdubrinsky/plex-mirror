package server

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"time"
)

// staticFS embeds the portal's client assets (htmx, stylesheet, and the
// self-hosted IBM Plex woff2 fonts under static/fonts/) into the binary so the
// distroless image needs no extra files and the portal works offline on the
// LAN. The pattern recurses into the fonts subdirectory.
//
//go:embed static
var staticFS embed.FS

func init() {
	// Go's built-in MIME table doesn't know .woff2, so http.FileServerFS would
	// fall back to content sniffing (application/octet-stream). Register it so
	// the embedded fonts are served with the correct type. Same for the PWA
	// manifest, which browsers ignore unless served as application/manifest+json.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// staticHandler serves the embedded assets under /static/. The Sub strips the
// "static" prefix so /static/app.css maps to static/app.css in the FS.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// The embed path is a compile-time constant, so this can never fail at
		// runtime; panic keeps the signature clean.
		panic(err)
	}
	return http.FileServerFS(sub)
}

// faviconHandler serves the multi-resolution .ico at the conventional root path
// (GET /favicon.ico) for browsers and bots that request it directly rather than
// honoring the <link rel="icon"> tags. The bytes are embedded, so this never
// touches disk; a stable Cache-Control keeps it out of every page's hot path.
func faviconHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := staticFS.ReadFile("static/icons/favicon.ico")
		if err != nil {
			// Embedded at compile time, so this is unreachable in a built binary.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, "favicon.ico", time.Time{}, bytes.NewReader(b))
	})
}
