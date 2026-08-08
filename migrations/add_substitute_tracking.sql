-- Substitute tracking for perpetual (recurring) leagues.
-- A one-night substitute is added as a registration for that session only. These
-- columns let the app auto-expire the sub at the next session build (their played
-- games + standings line survive, since matches carry player_id) and, for
-- fixed-partner leagues, restore the benched member's original pairing.
--   is_substitute  — true for a temporary sub registration.
--   substitute_for — the player_id this sub stood in for (to restore pairings).
-- Both are nullable/defaulted and column-guarded in code, so the app runs
-- unchanged until this migration is applied.
ALTER TABLE registrations
  ADD COLUMN IF NOT EXISTS is_substitute boolean NOT NULL DEFAULT false;

ALTER TABLE registrations
  ADD COLUMN IF NOT EXISTS substitute_for uuid;
