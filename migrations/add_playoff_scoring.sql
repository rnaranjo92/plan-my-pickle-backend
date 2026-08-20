-- Separate scoring for the PLAYOFFS.
--
-- Scoring was one setting for the whole event, so pool play and the bracket
-- always shared it. Organizers routinely don't want that: a common format is
-- fast pool games to 11 win-by-1 to get everyone playing, then the knockout to
-- 11 win-by-2 so the medal rounds are decided properly. There was no way to
-- express it, and the organizer ended up telling players a rule the app would
-- then reject at score entry.
--
-- NULL means "same as pool play", which is what every existing event gets — so
-- running this changes nothing until an organizer opts in.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent.

alter table events add column if not exists playoff_points_to_win integer;
alter table events add column if not exists playoff_win_by integer;

-- Verify (expect 0 until someone sets it):
--   select count(*) from events where playoff_win_by is not null;
