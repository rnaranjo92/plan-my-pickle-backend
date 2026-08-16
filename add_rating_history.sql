-- One row per player per day, whenever their DUPR rating is known to change.
--
-- Nothing in the schema keeps a rating history: dupr_connections holds the
-- CURRENT doubles/singles values and the webhook overwrites them. So a
-- rating-over-time chart — the one players actually want on their card — cannot
-- be drawn, and cannot be backfilled either. You can't recover history you
-- never stored, which is the whole argument for adding this before it's asked
-- for rather than after.
--
-- Deliberately tiny: an id, whose rating, what it was, and the day.
create table if not exists rating_history (
  dupr_id text not null,
  -- Denormalised so the chart can be read by account without a join. Empty when
  -- the connection isn't linked to a PlanMyPickle user yet.
  user_id text not null default '',
  doubles numeric,
  singles numeric,
  -- The DAY, not a timestamp. A rating that ticks twice in an afternoon is one
  -- point on a chart, and one row per day keeps this table small forever.
  day date not null,
  primary key (dupr_id, day)
);

create index if not exists rating_history_user_day_idx
  on rating_history (user_id, day);

comment on table rating_history is
  'Daily DUPR rating snapshots. Written on each webhook update; one row per '
  'player per day (the primary key upserts). Feeds the rating chart on a '
  'player card — history that exists only because it is recorded from now on.';
