-- Recurring-league schedule controls (perpetual events). The organizer can pause
-- the league (no play until resumed) or skip upcoming session(s) up to a date,
-- and reschedule the weekday/time (that just updates events.starts_at). The
-- header + feed reflect these. Safe pre-run (defaults keep every league running).
alter table events add column if not exists recur_paused boolean not null default false;
alter table events add column if not exists recur_skip_until date;
