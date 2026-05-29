package server

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
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
	// the embedded fonts are served with the correct type.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
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
