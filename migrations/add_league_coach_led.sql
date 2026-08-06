-- Coach-led leagues: the owner (a coach) auto-enrolls every registrant as a
-- coaching student, so they can give per-player feedback from the Coach tab.
-- coach_id is the enrolling coach (the league owner). Both default off/null, so
-- this is safe to ship before backfill — existing leagues stay non-coach-led.
alter table leagues add column if not exists coach_led boolean not null default false;
alter table leagues add column if not exists coach_id  uuid;
