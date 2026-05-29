#!/usr/bin/env -S uv run --quiet --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["plexapi>=4.15"]
# ///
"""Mint a reusable Plex auth token (X-Plex-Token) for plex-mirror.

Exchanges your plex.tv email + password (+ 2FA code, if enabled) for a
long-lived account token you can drop into PLEXMIRROR_PLEX_TOKEN. The token
is what every plex-mirror subcommand authenticates with; you only need to run
this once (re-run if you revoke the device or rotate the token).

Inputs (each: flag > env > interactive prompt):
  login     --login        / PLEX_USERNAME   plex.tv email or username
  password  (prompt only)  / PLEX_PASSWORD   read with no echo by default
  2FA code  --code         / PLEX_2FA        omit/blank if 2FA is off

Security:
  - The password is read with getpass (no echo, never placed in argv where
    `ps` or your shell history could capture it). Prefer the prompt over the
    PLEX_PASSWORD env var.
  - The printed token is a credential equivalent to your password for server
    access. Don't commit it or paste it into chat. Store it in your secrets
    manager / a non-committed .env and revoke it at
    https://app.plex.tv/desktop/#!/settings/devices if it leaks.

A stable client identifier is persisted to
~/.config/plex-mirror/client-id so repeated runs reuse one "authorized
device" entry on your account instead of creating a new one each time.
"""

import argparse
import getpass
import os
import sys
import uuid
from pathlib import Path

# plexapi reads its X-Plex-* identity headers from PLEXAPI_HEADER_* env vars at
# import time, so these must be set BEFORE importing plexapi below.
CLIENT_ID_PATH = Path(
    os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")
) / "plex-mirror" / "client-id"


def stable_client_id() -> str:
    """Return a persistent UUID for this machine's plex-mirror device entry."""
    try:
        existing = CLIENT_ID_PATH.read_text(encoding="utf-8").strip()
        if existing:
            return existing
    except FileNotFoundError:
        pass
    new_id = str(uuid.uuid4())
    CLIENT_ID_PATH.parent.mkdir(parents=True, exist_ok=True)
    CLIENT_ID_PATH.write_text(new_id + "\n", encoding="utf-8")
    return new_id


os.environ.setdefault("PLEXAPI_HEADER_IDENTIFIER", stable_client_id())
os.environ.setdefault("PLEXAPI_HEADER_PRODUCT", "plex-mirror")
os.environ.setdefault("PLEXAPI_HEADER_DEVICE_NAME", "plex-mirror")

from plexapi.myplex import MyPlexAccount  # noqa: E402
from plexapi.exceptions import TwoFactorRequired, Unauthorized  # noqa: E402


def prompt_if_blank(value: str | None, prompt: str, *, secret: bool = False) -> str:
    if value:
        return value
    if not sys.stdin.isatty():
        sys.exit(f"error: {prompt} not provided and stdin is not a TTY")
    if secret:
        return getpass.getpass(f"{prompt}: ")
    return input(f"{prompt}: ").strip()


def sign_in(login: str, password: str, code: str | None) -> MyPlexAccount:
    """Sign in, prompting for a 2FA code if the account requires one."""
    try:
        return MyPlexAccount(username=login, password=password, code=code or None)
    except TwoFactorRequired:
        if code:
            # A code was supplied but rejected — almost always an expired TOTP.
            sys.exit("error: 2FA code rejected (expired? codes rotate every 30s) — retry")
        retry_code = prompt_if_blank(None, "2FA code (6 digits)")
        try:
            return MyPlexAccount(username=login, password=password, code=retry_code)
        except Unauthorized as exc:
            sys.exit(f"error: sign-in failed after 2FA: {exc}")
    except Unauthorized as exc:
        sys.exit(f"error: sign-in failed (bad email/password?): {exc}")


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Mint a reusable Plex auth token for plex-mirror.",
    )
    parser.add_argument("--login", help="plex.tv email or username (or $PLEX_USERNAME)")
    parser.add_argument("--code", help="2FA verification code (or $PLEX_2FA)")
    parser.add_argument(
        "--export",
        action="store_true",
        help="print a ready-to-source `export PLEXMIRROR_PLEX_TOKEN=...` line only",
    )
    args = parser.parse_args()

    login = prompt_if_blank(args.login or os.environ.get("PLEX_USERNAME"), "Plex email/username")
    password = prompt_if_blank(os.environ.get("PLEX_PASSWORD"), "Plex password", secret=True)
    code = args.code or os.environ.get("PLEX_2FA")

    account = sign_in(login, password, code)
    token = account.authToken
    if not token:
        sys.exit("error: signed in but no authToken returned")

    if args.export:
        # Single-quote so shell metacharacters in the token can't bite; Plex
        # tokens are alnum so this is belt-and-suspenders.
        print(f"export PLEXMIRROR_PLEX_TOKEN='{token}'")
        return 0

    print(f"Signed in as {account.username} ({account.email})", file=sys.stderr)
    print(f"Device identifier: {os.environ['PLEXAPI_HEADER_IDENTIFIER']}", file=sys.stderr)
    print(file=sys.stderr)
    print("PLEXMIRROR_PLEX_TOKEN:", file=sys.stderr)
    print(token)  # token alone on stdout so `plex_token.py > /dev/null` etc. is clean
    print(file=sys.stderr)
    print("Add to your env (do NOT commit):", file=sys.stderr)
    print(f"  export PLEXMIRROR_PLEX_TOKEN='{token}'", file=sys.stderr)
    print("Revoke at https://app.plex.tv/desktop/#!/settings/devices if it leaks.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
