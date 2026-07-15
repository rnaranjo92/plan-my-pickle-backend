-- Allow a player to enter MULTIPLE divisions of the same event (e.g. Men's
-- Doubles 3.5 AND Mixed Doubles 3.5) — but never the same division twice.
--
-- The original schema enforced ONE registration per (event, player). Swap that
-- for per-(event, player, division) uniqueness. The backend's duplicate checks
-- are already scoped to the bracket to match this (and to guard the null-bracket
-- / no-division case, where Postgres treats NULLs as distinct in a unique index).
alter table registrations
  drop constraint if exists registrations_event_id_player_id_key;

create unique index if not exists registrations_event_player_bracket_uidx
  on registrations (event_id, player_id, bracket_id);
