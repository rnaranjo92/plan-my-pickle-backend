-- One human, one rung: a player can appear on a ladder only once.
--
-- THE BUG: AddLadderEntrant checks for an existing entrant with the same
-- player_id and returns it instead of inserting — but a CHECK in application
-- code is not a constraint. Two writers racing (the roster sync running while a
-- player self-joins by QR, which is exactly what happens at 7pm on a league
-- night) both read "not there", both insert, and the ladder now carries the same
-- person twice with their record split between the rows.
--
-- Partial index: only rows that HAVE a player_id are constrained. Walk-ups and
-- typed-in names have player_id NULL, there can be any number of them, and in
-- Postgres NULLs don't collide anyway — the WHERE clause makes that explicit
-- rather than incidental, and keeps the index small.
--
-- CONCURRENTLY so it doesn't take a write lock on a live ladder. Note this
-- cannot run inside a transaction block — if the Supabase SQL editor wraps
-- statements, run this one on its own.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run.
--
-- IF IT FAILS with a uniqueness error, the duplicates already exist. Find them:
--
--   select league_bracket_id, player_id, count(*), array_agg(display_name)
--   from ladder_entrants
--   where player_id is not null
--   group by 1, 2
--   having count(*) > 1;
--
-- then merge each pair in the app (Ladder → the entrant's ⋮ → Merge), which
-- moves the matches, challenges and session history onto the survivor, and run
-- this again.

create unique index concurrently if not exists
  ladder_entrants_one_player_per_division
  on ladder_entrants (league_bracket_id, player_id)
  where player_id is not null;

-- Verify (should list the index):
--   select indexname from pg_indexes
--   where tablename = 'ladder_entrants'
--     and indexname = 'ladder_entrants_one_player_per_division';
