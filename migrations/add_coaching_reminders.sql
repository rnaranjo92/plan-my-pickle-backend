-- Pre-session reminders + inactive-student re-engagement markers (idempotent so
-- the cron reminds/nudges once, not every tick).
alter table coaching_schedule
  add column if not exists reminded_at timestamptz;   -- a session's reminder sent
alter table coach_students
  add column if not exists nudged_at timestamptz;     -- last inactivity nudge sent
