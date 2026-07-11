-- Recurring "socials": an event with recur_interval_days > 0 is the head of a
-- series that auto-spawns its next occurrence every N days (7 = weekly, 14 =
-- biweekly, custom otherwise). recur_until caps how far the series generates
-- (NULL = open-ended). series_id links the head and every generated occurrence
-- of one recurring social (the head's series_id = its own id).
ALTER TABLE events
  ADD COLUMN IF NOT EXISTS recur_interval_days int NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS recur_until timestamptz,
  ADD COLUMN IF NOT EXISTS series_id uuid,
  -- series_cursor is the last slot the materializer has SPAWNED for this head.
  -- It only advances, so deleting an auto-created occurrence never resurrects
  -- it, and a dormant series never back-fills missed weeks. NULL = none spawned
  -- yet (anchor off the head's own starts_at).
  ADD COLUMN IF NOT EXISTS series_cursor timestamptz;

-- The materializer scans only active series heads.
CREATE INDEX IF NOT EXISTS events_recur_active
  ON events (recur_interval_days)
  WHERE recur_interval_days > 0;

-- Fast "latest occurrence in this series" lookups.
CREATE INDEX IF NOT EXISTS events_series_id
  ON events (series_id)
  WHERE series_id IS NOT NULL;
