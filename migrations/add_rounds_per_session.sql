-- Optional "rounds per session": an exact number of rounds to generate each time
-- a league/event schedule is built. 0 (default) = auto — derive per format
-- (singles/fixed = full round-robin, rotating doubles = ~N-1 clamped 3..12,
-- pools = 7), still honoring min_pool_rounds/max_pool_rounds. When > 0 it pins
-- every format to exactly that many rounds (a full round-robin is truncated; a
-- short one repeats matchups). Mainly for recurring leagues that want a fixed
-- "N rounds a week". The app treats a missing column as "auto" (columnReady
-- guard), so this can be run any time.

alter table events
  add column if not exists rounds_per_session integer not null default 0;
