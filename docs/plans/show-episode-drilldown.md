# Plan — Show → Season → Episode drill-down (downloadable shows)

- Status: **implemented 2026-05-29** (Plex live-verified end-to-end; Jellyfin
  unit-tested, not live-verified)
- Date: 2026-05-29
- Epic: glb-gdl (Plex → local mirror service)
- Related: closed glb-gdl.7 (portal), glb-gdl.9 (Plex pagination)

> Implemented as `source.ChildLister` + `Service.ListChildren` + portal
> drill-down + `list_children` MCP tool. Live check (Balerion): "The Crown"
> → 6 seasons → 10 episodes (5.3 GB MKV each), and queueing S01E01 created a
> `queued` row with no resolve error. The two "to confirm" items below are
> resolved: portal reuses `ActionCell` cleanly; Jellyfin `ParentId` path has a
> unit test (`recursive=false`) but was not exercised against a live server.

## Problem

Browsing a Plex/Jellyfin **show** library lists series, but a series is a
*container* with no media `Part` — only episodes carry downloadable bytes.
Today the item page shows a Queue button for any item on a download-capable
source, but:

- A series (`type=show`) has no parts, so `metadataToItem` yields size 0 / no
  container and `plex.ResolveDownloadURL` would fail with *"has no parts"*.
- There is **no navigation path** from a show to its episodes. The `Source`
  interface (`internal/source/source.go`) only exposes
  `ListItems(libraryID, …)` and `GetMetadata(itemID)` — nothing lists the
  children of an item.

Net effect: movies (top-level leaves) are downloadable; shows are dead ends.

Confirmed live (Balerion, 2026-05-29): item `114203` = "The Crown",
`type=show`, 6 seasons, 60 episodes. Episode S01E01 (`ratingKey 114205`) is a
5.6 GB `.mkv` at `/library/parts/227937/…` — i.e. episodes *are* normal
downloadable leaves; only the traversal is missing.

The backend is already half-ready: `download/layout.go` builds
`shows/Show/Season NN/Show - sNNeNN - Title.ext` and `metadataToItem` stamps
`ShowTitle`/`SeasonNumber`/`EpisodeNumber` for episodes.

## Approach

### 1. Source capability: list children

Add an **optional** interface mirroring `DownloadResolver` (keep `Source` small;
not every backend must implement it):

```go
// ListChildren returns the direct children of a container item (show→seasons,
// season→episodes). Returns ErrUnsupported for leaf items / backends without
// a hierarchy.
type ChildLister interface {
    ListChildren(ctx context.Context, itemID string, opts ListOptions) ([]Item, error)
}
```

- **Plex** (`internal/source/plex`): `GET /library/metadata/{id}/children`
  returns seasons for a show and episodes for a season (same `metadataToItem`
  mapping; episodes already populate part/size + sNNeNN). One method covers
  both levels — Plex's `/children` is uniform.

  **Verified live (Balerion, 2026-05-29):**
  - `/library/metadata/114203/children` → `viewGroup=season`, 6 items, each
    `type=season` with ratingKey/index/title.
  - `/library/metadata/114204/children` → `viewGroup=episode`, 10 items, each
    `type=episode` with `grandparentTitle`/`parentIndex`/`index` **and** a full
    `Media[].Part[]` (key, size, container).
  - ⇒ Episode children carry parts directly, so `metadataToItem` maps them with
    no per-episode `GetMetadata` round-trip. This was the main risk; it's clear.
- **Jellyfin** (`internal/source/jellyfin`): `GetItems(...).ParentId(itemID)`
  with `Recursive(false)` — it already uses `ParentId` for library listing, so
  this is a parameter change, not new plumbing.

### 2. Service method

`Service.ListChildren(ctx, source, itemID, limit, offset)` — type-asserts the
source to `ChildLister`, returns `ErrDownloadUnavailable`-style sentinel if not
supported. One-liner, same shape as the existing `ListItems`.

### 3. Portal UI

- Item page: if `Item.Kind` is `show`/`season`, render children (seasons or
  episodes) instead of (or above) a Queue button. Episodes get the normal
  `ActionCell` Queue button keyed on the **episode** ratingKey.
- Optional convenience: "Queue all episodes" on a show/season → fan out
  `QueueDownload` per episode. (Defer if it complicates the first cut.)
- Breadcrumb: Library → Show → Season so the back-path isn't a dead end
  (ties into the UX-flow concerns the portal already tries to avoid).

### 4. MCP

Add `list_children(source, item_id)` tool — one-line call into
`Service.ListChildren`, same as the other nine tools. `queue_download` already
takes an item id, so queueing an episode needs no change.

## Out of scope / open questions

- **Music** (artist → album → track) falls out of the same `/children`
  traversal for free on Plex; verify Jellyfin parity if music is wanted.
- "Queue whole show" could create many rows fast — decide whether to cap or
  confirm in the UI.
- Eviction granularity stays per-file (per episode); no change.

## Confidence — verified vs. to-confirm-while-building

- **Verified:** Plex `/children` shapes + episodes-carry-parts (above);
  `metadataToItem` already handles episode fields; `layout.go` already builds
  episode paths. The Plex data layer is implementation-ready.
- **Sketch, confirm during build (not blockers):**
  1. **Jellyfin** `GetItems().ParentId(seasonId).Recursive(false)` returning
     episodes — inferred from the existing `ParentId` library-listing path, not
     tested against a live Jellyfin.
  2. **Portal composition** — the plan assumes the existing `ActionCell` can be
     reused per-episode under the item page; `item.templ` / `ItemDetailVM` have
     not been read to confirm the cleanest insertion point.

## Test plan

- Plex adapter: `/library/metadata/{id}/children` → seasons, then episodes;
  assert episode items carry sNNeNN + part size (fixtures from real shape).
- Jellyfin adapter: `ParentId` is sent; children mapped.
- Service: `ListChildren` returns `ErrUnsupported` sentinel for a source
  without the capability.
- Portal: show item page renders episode rows with Queue actions.
