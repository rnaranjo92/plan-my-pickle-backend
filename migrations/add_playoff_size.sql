-- "Finish with a playoff" — a per-event DRAW SIZE for an auto-built playoff.
--
--   0  = off (manual "Build playoff" only, the default)
--   4  = semifinals  (top 4 → medal bracket: gold, silver, bronze)
--   8  = quarterfinals (top 8)
--   16 = round of 16 (top 16)
--
-- When set, the bracket auto-builds from the pool/round-robin standings the
-- moment the last pool game is scored — no manual step. It CLAMPS to the teams
-- actually present, so choosing Top 8 with only 6 teams builds a 6-in-8 draw
-- (two byes) rather than erroring. Only round-robin / pools_playoff produce
-- standings, so it's inert on pure elimination draws.
--
-- This replaces the fixed top-4 that pools_playoff used to always auto-seed:
-- pick 8 and you get quarterfinals instead of just semifinals.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent.
-- columnReady-guarded, so create/edit keep working until this runs (the size
-- just can't be set yet). Existing pools_playoff events keep their top-4 finish
-- (playoff_size defaults to 0 → legacy path).

alter table events
  add column if not exists playoff_size integer not null default 0;

-- Verify:
--   select playoff_size, count(*) from events group by playoff_size;
