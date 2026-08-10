-- Ladder rotation SCORECARD: one score per player per round.
--
-- The rotation session previously recorded only who won each court
-- (rotation_round_courts.winner) and ranked standings by wins. Organizers asked
-- for a plain scorecard instead: players down the side, rounds across the top,
-- type each player's score. Standings then rank by TOTAL POINTS.
--
-- Deliberately keyed by (session, round, player) rather than by court: the
-- organizer shouldn't have to care which court someone was on, and this keeps
-- the existing court/pairing machinery completely untouched.
--
-- The backend probes this table via columnReady, so deploying before this runs
-- is safe — the scorecard simply stays hidden until the table exists.
--
-- HOW TO RUN: Supabase dashboard -> SQL Editor -> paste -> Run.

create table if not exists rotation_round_scores (
  id                 uuid primary key default gen_random_uuid(),
  session_id         uuid        not null,
  round              int         not null,
  rotation_player_id uuid        not null,
  score              int,
  created_at         timestamptz not null default now(),
  updated_at         timestamptz not null default now(),
  unique (session_id, round, rotation_player_id)
);

-- The grid reads a whole session at once, and standings sum per player.
create index if not exists rotation_round_scores_session_idx
  on rotation_round_scores (session_id, round);
create index if not exists rotation_round_scores_player_idx
  on rotation_round_scores (rotation_player_id);

alter table rotation_round_scores enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy, matching
-- the other rotation tables.

-- Verify — expect the table with 0 rows.
select count(*) as rotation_round_scores_rows from rotation_round_scores;
