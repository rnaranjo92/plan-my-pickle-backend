-- Live scorebug values for the broadcast overlay.
--
-- These hold the RUNNING game score of an in-progress match, pushed point-by-
-- point from the court scorer page so the OBS/Streamlabs overlay can show a
-- live score. They are deliberately SEPARATE from team1_score/team2_score
-- (which are the final, standings-affecting totals written only when a match is
-- marked completed) so live in-progress updates can never corrupt results or
-- point-differential math. Null until a scorekeeper pushes the first update.
alter table matches
  add column if not exists live_team1 integer,
  add column if not exists live_team2 integer,
  add column if not exists live_updated_at timestamptz;
