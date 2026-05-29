# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

plex-mirror is a single Go binary that pulls a curated subset of a remote (shared) Plex
server onto local storage for a co-located Jellyfin instance to serve. It runs as a
long-lived service exposing **two faces over one core**: a browser portal (templ + HTMX)
and an MCP server (streamable HTTP under `/mcp`). The same binary is also a CLI and its
own Docker healthcheck probe. Designed for homelab/single-tenant use behind Traefik.

## Commands

```bash
go build ./...                         # build everything
go vet ./...                           # vet (run before pushing; CI runs it)
go test ./...                          # full test suite
go test ./internal/download -run TestQueue   # single package / single test (-run regex)

go run ./cmd/plex-mirror               # run server mode locally (needs PLEXMIRROR_* env)

# CLI subcommands (positional; anything starting with '-' is server-mode flags instead):
go run ./cmd/plex-mirror dump --source=plex            # list libraries (JSON to stdout)
go run ./cmd/plex-mirror dump --source=plex --library=ID
go run ./cmd/plex-mirror download --source=plex --item=RATINGKEY   # one-shot, resumable
go run ./cmd/plex-mirror evict-now [--dry-run]         # force an eviction pass

docker compose up --build              # build + run the container (reads sibling .env)
```

CI (`.github/workflows/ci.yml`) is just `go build` + `go vet` + `go test` on Go 1.25.

### Editing the portal UI (templ)

The portal views live in `internal/server/views/*.templ` and compile to committed
`*_templ.go` files (e.g. `browse.templ` → `browse_templ.go`). **After editing any
`.templ` file you must regenerate** or your changes won't take effect:

```bash
go run github.com/a-h/templ/cmd/templ generate    # from repo root; walks recursively
```

There is no Makefile; the `//go:generate templ generate` directive lives in
`internal/server/views/helpers.go`.

## Architecture

### Two faces, one core

Both faces are thin adapters over `internal/service` — **no business logic lives in the
transport layers**:

- `internal/service` (`Service`) owns the configured sources, the storage manager, and
  the download engine. Its methods are transport-agnostic: they take a `context.Context`
  + primitive args and return typed DTOs or sentinel errors (`ErrSourceNotFound`,
  `ErrDownloadUnavailable`, `ErrItemNotFound`, `ErrCannotCancel`).
- `internal/server` is the HTTP layer: `/healthz`, the cookie-auth browser portal
  (`portal.go`, returning templ fragments for HTMX swaps), and it mounts the MCP handler.
- `internal/mcp` registers MCP tools (`list_sources`, `list_libraries`, `list_items`,
  `get_item`, `queue_download`, `download_status`, `list_mirrored`, `storage_stats`,
  `evict`) — each a one-line call into `Service`.

When adding a capability, implement it once on `Service`, then expose it from both faces.

### The source abstraction (`internal/source`)

`Source` is the browse interface (`ListLibraries`, `ListItems`, `GetMetadata`).
`DownloadResolver` is a **separate optional capability** — only Plex hands back
downloadable bytes; Jellyfin is mirror-inventory + a post-import scan trigger only.

- `internal/source/plex` implements both `Source` and `DownloadResolver`.
- `internal/source/jellyfin` implements `Source`; its `scanner.go` provides the
  `Refresh` used to trigger a library rescan after a download lands.
- **Adapters must not leak backend types** (Plex XML, Jellyfin OpenAPI structs) across
  the package boundary — only `source.*` values cross. Adapters wrap the package
  sentinels (`source.ErrAuth`, `ErrNotFound`, `ErrNetwork`, `ErrUnsupported`) so callers
  can `errors.Is` them.

Configured sources are non-fatal if creds are absent: the service boots browse-only, and
the download engine is simply disabled when Plex is missing.

### The `items` table is the single source of truth for mirror state

`internal/db` opens a pure-Go SQLite DB (`modernc.org/sqlite`, so the binary is CGO-free
and static) with WAL + `MaxOpenConns(1)`, and runs embedded migrations
(`internal/db/migrations/*.sql`) on boot via a `schema_migrations` ledger.

The `items` table tracks each mirror's lifecycle:
`queued → downloading → ready → evicted` (or `error`). State lives in the DB so the
engine survives process restarts. `UNIQUE(source, source_key)` drives all upsert/dedup.
`MirrorItem` (in `service/mirror.go`) is the local-DB view; keep `itemColumns` and
`scanItem` in lockstep when changing the schema.

### Download engine (`internal/download`)

Resumable HTTP `Range` GET against a `DownloadResolver`:

- Writes to `<MediaRoot>/.partials/<sha256(source:key)[:16]>.tmp`; on completion does an
  **atomic rename** into the Jellyfin layout (same filesystem) so Jellyfin never indexes
  a partial file. The partial path is deterministic, so resume needs no DB column.
- **Filesystem is truth**: `bytes_done` is reconciled from the on-disk partial size at
  the start of every `Download`, and a `200 OK` (server ignored `Range`) truncates and
  restarts from byte 0.
- Distinguishes `permanentError` (4xx, auth, integrity/size mismatch — no retry) from
  transient errors (5xx, transport — exponential backoff with jitter).
- `ResetStaleDownloads` flips orphaned `downloading` rows back to `queued` on startup.
- `layout.go` builds Jellyfin-friendly paths (`movies/Title (Year).ext`,
  `shows/Show/Season NN/Show - sNNeNN - Title.ext`) and `sanitize`s titles for the FS.

### Storage manager (`internal/storage`)

Eviction by soft/hard byte caps. Two critical invariants:

- **Path-escape guard** (`pathIsUnderRoot`): the manager refuses to `os.Remove` anything
  not resolving under `MediaRoot`, so a tampered DB row can never turn it into a
  delete-anywhere primitive.
- Eviction ordering is **age-only for now** (`completed_at ASC`); there's a `TODO(LRU)`
  to switch to `last_accessed` once Jellyfin played-state is wired in.

The sweeper (`RunSweeper`) and the download daemon (`Engine.Run`) are the two background
workers started by `Service.Start`; a zero interval disables each.

### Live runtime + hot reload

The configurable bits — config, sources, engine, storage manager — live in an immutable
`runtime` struct held in an `atomic.Pointer[runtime]` on `Service`. Operation methods take
one snapshot at entry (`rt := s.now()`) and reuse it, so an in-flight request always sees a
consistent `(cfg, sources, engine, storage)` tuple. `store` and `settings` are immutable for
the Service's lifetime and stay direct fields.

`Service.Reload` rebuilds the runtime from env+DB settings and swaps it **live, no restart**
(glb-gdl.13). It builds the new runtime *outside* the lock (Plex discovery is slow), then under
`s.mu` cancels+waits the old worker generation **before** swapping so the old engine isn't
writing `.partials` when the new one starts; an abandoned in-flight download is reset
`downloading→queued` by the new engine's `ResetStaleDownloads` and resumes from the partial.

### Config & auth

Config is env-bootstrapped (prefixed `PLEXMIRROR_`, `internal/config`) and then overlaid by a
DB-backed settings layer (`internal/settings`, the `settings` table) editable from the portal's
**Settings** page — env seeds the defaults, a stored value overrides it, and saving applies live
via `Service.Reload`. `settings.Effective(base, vals)` does the overlay + the same validation as
`config.Load`. Sizes accept `K/M/G/T` suffixes (1024-based). Local dev/Docker reads a sibling
`.env` (gitignored). `MediaRoot`, `AuthToken`, and `SecretKey` stay **env-only** (the first two
could lock the operator out; the third protects the rest).

Source tokens are encrypted at rest (AES-256-GCM, `crypto.go`) under a key stretched from
`PLEXMIRROR_SECRET_KEY` with **scrypt + a per-store random salt** (the salt lives in the
`settings` table under the reserved `$kdf.salt` key); empty key = plaintext fallback, and a key
added later re-encrypts existing plaintext secrets on boot (`EncryptPlaintextSecrets`). Secrets
are **never** rendered back to the UI or returned by the MCP `get_config` tool — the settings
page shows only a tri-state badge (set / not set / unreadable).

Auth is an optional static token (`PLEXMIRROR_AUTH_TOKEN`): the portal uses a cookie,
`/mcp` uses a `Bearer` header, both constant-time compared. **Empty token = no app-level
auth**, intended for deployments with Traefik forward-auth in front. `/healthz` and
`/static` are always open.

## Plex token gotcha

A **shared** Plex server 401s your plex.tv account token — `PLEXMIRROR_PLEX_TOKEN` must
be the **per-resource access token** for that specific server. Use `plex_token.py` /
`probe_plex.py` (repo root) to obtain and verify it. See `docs/adr/0001-library-picks.md`
for the library choices and the empirically-confirmed Plex download behavior
(`206 Partial Content`, resumable ranges).
