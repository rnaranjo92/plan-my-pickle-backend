-- Rotation ladder: where does each player START?
--
-- Today round 1 is seeded strongest-first by self-rating, which is the right
-- default but the wrong answer for two real cases Michelle raised: a social
-- night where nobody has rated themselves (everyone ties, so "seeding" is
-- really just insertion order), and an organizer who knows the room and wants
-- to place people by hand.
--
-- One mechanism covers both. A player may be given a starting court; at start
-- the roster is ordered by that court first and self-rating only for whoever
-- was left unplaced. "Shuffle" is not a separate mode — it simply fills these
-- in at random. NULL everywhere reproduces today's rating seed exactly, so
-- every existing session is untouched.
alter table rotation_players
  add column if not exists start_court int;
