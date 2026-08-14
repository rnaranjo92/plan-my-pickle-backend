-- Rotation sessions: an optional end condition.
--
-- A rotation night has never had one — it runs until the organizer taps End,
-- which matches how "up and down the river" is played but means a session
-- nobody closes sits live all night, buzzing every few minutes on whatever
-- device is still watching.
--
-- Two independent limits, both optional, either or both may be set:
--   planned_rounds  "tonight is 10 rounds"  (0 = no limit, the old behaviour)
--   stop_at         "we have the courts until 8pm"
--
-- The engine stops when EITHER is reached: it won't start round 11, and it
-- won't start a round that can't finish before stop_at. Neither ever ends a
-- round early — the round being played always finishes.
alter table rotation_sessions
  add column if not exists planned_rounds integer not null default 0,
  add column if not exists stop_at timestamptz;

comment on column rotation_sessions.planned_rounds is
  'Stop after this many rounds. 0 = run until the organizer ends it.';
comment on column rotation_sessions.stop_at is
  'Do not start a round that would run past this time. NULL = no time limit.';
