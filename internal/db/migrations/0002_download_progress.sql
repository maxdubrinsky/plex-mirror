-- Phase 2 (download engine) adds a last_progress_at heartbeat so the portal and
-- sweeper can tell stalled downloads from healthy ones without scraping logs.
-- bytes_done already exists; we just need the timestamp companion.
ALTER TABLE items ADD COLUMN last_progress_at INTEGER;
