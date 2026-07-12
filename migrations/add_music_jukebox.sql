-- Spotify jukebox: a player-driven music queue for the live TV scoreboard.
-- Players add real Spotify tracks (searched via the organizer's connection);
-- the TV plays down the queue and ducks for court calls. All access is through
-- the Go backend (service-role) — RLS is ON with no anon policy so neither
-- table is reachable via the anon key.

-- The organizer's Spotify link. One row per organizer account (the event uses
-- its owner's connection). Tokens are secrets — never exposed to players; the
-- backend proxies search and mints short-lived tokens only for the owner's TV.
CREATE TABLE IF NOT EXISTS spotify_connections (
  user_id       uuid PRIMARY KEY,
  refresh_token text NOT NULL,
  access_token  text,
  expires_at    timestamptz,
  scope         text,
  spotify_user  text,           -- Spotify account display id (for the UI)
  product       text,           -- "premium" | "free" — Web Playback needs premium
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE spotify_connections ENABLE ROW LEVEL SECURITY;

-- The shared per-event music queue.
CREATE TABLE IF NOT EXISTS music_queue (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id         uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  track_uri        text NOT NULL,          -- spotify:track:...
  track_name       text NOT NULL,
  artist           text,
  album_art        text,
  duration_ms      integer,
  added_by_user_id uuid,                   -- null = added by the organizer/kiosk
  added_by_name    text,
  -- queued | approved | playing | played | skipped
  status           text NOT NULL DEFAULT 'queued',
  created_at       timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE music_queue ENABLE ROW LEVEL SECURITY;

-- The TV polls "what's next" (FIFO within an event); this index keeps that cheap.
CREATE INDEX IF NOT EXISTS music_queue_event_status_idx
  ON music_queue (event_id, status, created_at);

-- Per-event jukebox settings.
ALTER TABLE events
  ADD COLUMN IF NOT EXISTS music_enabled boolean NOT NULL DEFAULT false;
-- When true, a player's track lands as 'queued' and only plays after the
-- organizer approves it (else players' adds go straight to the play queue).
ALTER TABLE events
  ADD COLUMN IF NOT EXISTS music_require_approval boolean NOT NULL DEFAULT false;
