-- Phase 6 (glb-gdl.13): DB-backed configuration set from the portal settings
-- page, layered on top of the env bootstrap. A simple key/value store keyed by
-- a dotted setting name (e.g. "plex.url", "storage.hard_cap"). Secret values
-- (source tokens) are stored AES-GCM-encrypted with a "gcm:" prefix when
-- PLEXMIRROR_SECRET_KEY is set, else plaintext; is_secret flags them so the
-- portal never renders them back.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    is_secret  INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);
