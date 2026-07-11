-- Behind-schedule flag: remembers the last delay (minutes) an organizer
-- acknowledged, so the banner stays quiet until the delay grows materially
-- worse. 0 = never acknowledged. Nothing player-facing; owner-only endpoint.
ALTER TABLE events
  ADD COLUMN IF NOT EXISTS schedule_ack_minutes integer NOT NULL DEFAULT 0;
