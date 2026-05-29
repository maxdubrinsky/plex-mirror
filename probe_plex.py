#!/usr/bin/env -S uv run --quiet --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["plexapi>=4.15", "requests>=2.31"]
# ///
"""Quick probe: can we download originals from a friend's shared Plex server?

Auth modes (env), in priority order:
  Mode A (direct):       PLEX_URL + PLEX_TOKEN  — bypass plex.tv entirely.
  Mode B (token only):   PLEX_TOKEN             — query plex.tv for shared servers.
  Mode C (user+pass):    PLEX_USERNAME + PLEX_PASSWORD [+ PLEX_2FA]
                           — 2FA: pass the current TOTP code; plexapi appends
                             it to the password.

  Optional: PLEX_SERVER_NAME picks a specific shared server in modes B/C.
"""

import os
import sys
import requests
from plexapi.myplex import MyPlexAccount
from plexapi.server import PlexServer


def connect():
    url = os.environ.get("PLEX_URL")
    token = os.environ.get("PLEX_TOKEN")
    user = os.environ.get("PLEX_USERNAME")
    pw = os.environ.get("PLEX_PASSWORD")
    twofa = os.environ.get("PLEX_2FA")
    name_filter = os.environ.get("PLEX_SERVER_NAME")

    if url and token:
        print(f"[mode] direct: {url}")
        return PlexServer(url, token), token

    if token and not (user and pw):
        print("[mode] account-via-token")
        account = MyPlexAccount(token=token)
    elif user and pw:
        if twofa:
            print(f"[mode] account+2FA: {user}")
            account = MyPlexAccount(user, pw, code=twofa)
        else:
            print(f"[mode] account: {user}")
            account = MyPlexAccount(user, pw)
    else:
        sys.exit(
            "Set one of:\n"
            "  PLEX_URL + PLEX_TOKEN              (direct, no plex.tv)\n"
            "  PLEX_TOKEN                         (token-only account lookup)\n"
            "  PLEX_USERNAME + PLEX_PASSWORD [+ PLEX_2FA]\n"
            "Optional: PLEX_SERVER_NAME"
        )
    resources = [r for r in account.resources() if "server" in r.provides]
    if not resources:
        sys.exit("No Plex servers visible on this account.")
    print(f"[resources] {len(resources)} server(s):")
    for r in resources:
        owned = "owned" if r.owned else "shared"
        print(f"  - {r.name}  ({owned}, {r.product} {r.productVersion})")
    target = next((r for r in resources if name_filter and r.name == name_filter), None)
    if not target:
        target = resources[0]
        if name_filter:
            print(f"[warn] PLEX_SERVER_NAME={name_filter!r} not found; using {target.name}")
    print(f"[connect] {target.name}")
    server = target.connect()  # picks best URI
    return server, server._token


def pick_item(server):
    """Return a (Video, MediaPart) tuple — smallest video we can find."""
    sections = server.library.sections()
    print(f"[libraries] {len(sections)}:")
    for s in sections:
        print(f"  - {s.title!r}  type={s.type}  agent={s.agent}")
    video_sections = [s for s in sections if s.type in ("movie", "show")]
    if not video_sections:
        sys.exit("No movie/show libraries on this server.")

    # Walk one library, find smallest item by file size to keep the probe cheap.
    sec = video_sections[0]
    print(f"[probe-library] using {sec.title!r}")
    candidates = []
    if sec.type == "movie":
        items = sec.all()[:200]
    else:
        items = []
        for show in sec.all()[:20]:
            items.extend(show.episodes()[:5])
    for it in items:
        for media in getattr(it, "media", []) or []:
            for part in media.parts:
                if part.size:
                    candidates.append((part.size, it, part))
    if not candidates:
        sys.exit("No items with size metadata found.")
    candidates.sort(key=lambda x: x[0])
    size, item, part = candidates[0]
    title = getattr(item, "title", "?")
    print(f"[pick] {title!r}  size={size:,} bytes  part.file={part.file!r}")
    print(f"        part.key={part.key!r}")
    return item, part


def try_range_download(server_url, token, part):
    # Plex's documented download path: /library/parts/<id>/<ts>/file.<ext>?download=1
    # part.key is the path already, e.g. /library/parts/12345/167.../file.mkv
    url = f"{server_url.rstrip('/')}{part.key}"
    params = {"download": "1", "X-Plex-Token": token}
    headers = {"Range": "bytes=0-1048575"}  # first 1 MiB
    print(f"[GET] {url}  Range: {headers['Range']}")
    r = requests.get(url, params=params, headers=headers, stream=True, timeout=30)
    print(f"[resp] status={r.status_code}  content-length={r.headers.get('Content-Length')}")
    print(f"       content-range={r.headers.get('Content-Range')}")
    print(f"       content-type={r.headers.get('Content-Type')}")
    print(f"       accept-ranges={r.headers.get('Accept-Ranges')}")
    if r.status_code in (200, 206):
        chunk = next(r.iter_content(64), b"")
        magic = chunk[:16].hex()
        print(f"[ok] first 16 bytes (hex): {magic}")
        # Common video magics: matroska=1A45DFA3, mp4=...66747970(ftyp at offset 4)
        if chunk[:4] == b"\x1a\x45\xdf\xa3":
            print("       (matroska/webm container)")
        elif b"ftyp" in chunk[:32]:
            print("       (mp4/mov container)")
        return True
    elif r.status_code == 401:
        print("[fail] 401 — token rejected or download disabled by share owner.")
    elif r.status_code == 403:
        print("[fail] 403 — 'Allow Downloads' likely off for your account on this share.")
    else:
        print(f"[fail] unexpected status; body[:300]={r.text[:300]!r}")
    return False


def main():
    server, token = connect()
    print(f"[server] {server.friendlyName}  v{server.version}  url={server._baseurl}")
    _, part = pick_item(server)
    ok = try_range_download(server._baseurl, token, part)
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
