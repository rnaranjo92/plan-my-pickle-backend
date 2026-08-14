-- Pause / resume a live rotation session.
--
-- The 'paused' status was already read in three places (advance, end, the live
-- guard) but nothing ever WROTE it, so an organizer could never reach it. A
-- night gets interrupted — rain, an injury, a court double-booked, somebody's
-- kid — and today the only options are to let the round clock run out and
-- auto-advance people who never played, or end the session outright.
--
-- paused_at is when the clock stopped. Resuming pushes round_ends_at forward by
-- exactly how long the pause lasted, so the round keeps the time it had left
-- rather than restarting or expiring while everyone stood around.
alter table rotation_sessions
  add column if not exists paused_at timestamptz;

comment on column rotation_sessions.paused_at is
  'When the round clock was paused. NULL while running. Resume shifts round_ends_at by now() - paused_at.';
