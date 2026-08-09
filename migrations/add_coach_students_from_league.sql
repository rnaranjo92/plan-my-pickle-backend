-- Origin marker for a coaching roster row: TRUE when the row was created by a
-- coach-led LEAGUE auto-enroll, FALSE when the coach added the student manually.
-- Only league-created rows may be auto-removed when a player leaves the league;
-- a manually-added (or manually-claimed) student shares the same (coach,contact)
-- row and must NEVER be torn down by a league-leave — doing so would cascade
-- coaching_videos and destroy the coach's own clip/feedback history.
--
-- Defaults false, so existing rows are treated as protected (manual). Safe to
-- run any time; the code is columnReady-gated and simply skips league cleanup
-- until this exists.
alter table coach_students
  add column if not exists from_league boolean not null default false;
