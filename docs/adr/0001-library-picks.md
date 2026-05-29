# ADR 0001 — Library picks

- Status: accepted
- Date: 2026-05-27
- Tickets: glb-gdl.2

## Context

Plex-mirror is a single-binary Go service with two faces (web portal + MCP
server) that pulls a curated subset of a remote shared Plex onto local storage
for a Jellyfin instance to serve. We need to lock in the external Go libraries
before Phase 1 work begins so the adapter authors don't have to re-litigate
the choice mid-implementation.

## Decisions

### Plex client: `jrudio/go-plex-client` (browse) + hand-rolled HTTP (download)

- Browse surface used: `GetLibraries`, `GetLibraryContent`, `GetMetadata`,
  `Search`.
- Download uses hand-rolled `net/http` with `Range` because the library's
  thin wrapper doesn't model partial / resumable transfers well and we want
  full control of retry / cancellation. Plex download is just a `GET` against
  `{server}{Part.key}?download=1&X-Plex-Token=...` with `Range: bytes=N-`.
- Confirmed empirically 2026-05-27 against the actual share: `Allow Downloads`
  is on, `206 Partial Content` works, byte ranges resume cleanly. See
  `probe_plex.py` for the probe and the saved sample headers.
- The library is wrapped behind our `source.Source` interface so it is
  swappable later if we hit limits (e.g., a future Plex API revision).

### Jellyfin client: `sj14/jellyfin-go`

- OpenAPI-generated client, version 0.4.3 (April 2026). Auth header:
  `Authorization: MediaBrowser Token=...`.
- Surface is comprehensive because it's codegen — UserViews, Items, ImageUrl
  builders, played-state on items.
- Alternative `shamelin/go-jellyfin-api-client` is the same OpenAPI-generator
  origin but lags `sj14` by several releases.

### MCP SDK: `mark3labs/mcp-go` via `server.NewStreamableHTTPServer`

- Streamable HTTP is the current transport in the MCP spec; the server sits
  behind Traefik next to the web portal.
- Alternative `modelcontextprotocol/go-sdk` (official) is viable but
  `mark3labs` has more snippets and live community use today. We can swap
  later if the official SDK pulls ahead — the MCP tool surface we build will
  not be SDK-specific.

### Templating: `a-h/templ` + HTMX

- `templ` gives us typed templates compiled into Go. HTMX is the interaction
  layer for the portal so we don't ship a JS bundle. `templUI` is a candidate
  for prebuilt components but is not adopted up front.

## Consequences

- Phase 1a (`glb-gdl.3`) and Phase 1b (`glb-gdl.4`) consume the Plex and
  Jellyfin libraries respectively.
- Phase 2 (`glb-gdl.6`) consumes `source.DownloadResolver` (implemented only
  by the Plex adapter) and does the actual HTTP `Range` GET itself.
- Phase 5 (`glb-gdl.8`) imports `mark3labs/mcp-go`.
- Phase 4 (`glb-gdl.7`) imports `a-h/templ`.

## Notes

- Plex API returns XML by default; `jrudio/go-plex-client` handles parsing.
  Our adapter must not leak XML types — only `source.*` values cross the
  package boundary.
- Jellyfin used to ship two source-of-truth OpenAPI specs (stable vs
  unstable). `sj14/jellyfin-go` targets the stable spec; we are not relying
  on any preview endpoints.
