-- Rotation ladder: where does a LOSING pair go?
--
-- Two ways clubs run "up & down the river", chosen when the ladder is created
-- and fixed for the session:
--
--   'down'  losers drop one court (the classic river). The bottom court's losers
--           stay, and are the ones who rotate off when players are waiting.
--   'stay'  losers hold their court and only winners climb — except the TOP
--           court's losers, who fall all the way to the bottom. That fall is
--           what keeps every court at four: court 1 takes its own winners plus
--           the climbers from court 2, each middle court takes its own losers
--           plus climbers from below, and the bottom takes its own losers plus
--           the pair dropped from the top.
--
-- Defaults to 'down', which is the behaviour every existing ladder already has,
-- so this migration changes nothing until an organizer picks the other mode.
alter table leagues
  add column if not exists ladder_loser_mode text not null default 'down';

alter table leagues
  drop constraint if exists leagues_ladder_loser_mode_check;

alter table leagues
  add constraint leagues_ladder_loser_mode_check
  check (ladder_loser_mode in ('down', 'stay'));
