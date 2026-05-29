-- Initial state schema. Phase 0 lays down the table that later phases (download
-- engine + storage manager) will fill in. Keeping it here so the migration
-- runner has something to apply and idempotency is exercised on first boot.
CREATE TABLE IF NOT EXISTS items (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    source          TEXT    NOT NULL,                  -- e.g. "plex:<server-id>"
    source_key      TEXT    NOT NULL,                  -- Plex Part.key or analogue
    title           TEXT    NOT NULL,
    container       TEXT,                              -- mkv, mp4, ...
    size_bytes      INTEGER,
    local_path      TEXT,                              -- final path under MEDIA_ROOT once imported
    status          TEXT    NOT NULL DEFAULT 'queued', -- queued|downloading|ready|evicted|error
    bytes_done      INTEGER NOT NULL DEFAULT 0,
    error           TEXT,
    queued_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    started_at      INTEGER,
    completed_at    INTEGER,
    last_accessed   INTEGER,
    UNIQUE(source, source_key)
);

CREATE INDEX IF NOT EXISTS items_status_idx        ON items(status);
CREATE INDEX IF NOT EXISTS items_last_accessed_idx ON items(last_accessed);
