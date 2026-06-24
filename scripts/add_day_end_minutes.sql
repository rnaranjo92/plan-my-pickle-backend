-- Per-day court closing times for an event's schedule.
-- A jsonb array of minutes-from-midnight, indexed by tournament day
-- (e.g. [1260, 1080] = day 1 closes 21:00, day 2 closes 18:00). -1 in a slot
-- means "no closing time that day". Takes precedence over day_cap_minutes.
-- A game that wouldn't FINISH before its day's close rolls to the next day;
-- on the last/only day it's flagged as running past closing.
alter table events add column if not exists day_end_minutes jsonb;
