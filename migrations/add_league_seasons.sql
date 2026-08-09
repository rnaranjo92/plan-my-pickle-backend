-- Perpetual-league SEASONS. A recurring league runs as one ongoing event whose
-- standings accumulate forever. "Start new season" archives the current
-- standings and moves the live leaderboard's cutoff forward — without deleting
-- any matches (history + DUPR stay whole).
--
-- season_started_at is the cutoff: only rounds created on/after it count toward
-- the live standings (see season.go standingRowsSince). NULL = all-time (the
-- pre-season behavior — every existing league is unaffected). The app treats a
-- missing column / table as "seasons off" (columnReady guard), so this is safe
-- to run any time.

alter table events
  add column if not exists season_number     integer     not null default 1;
alter table events
  add column if not exists season_started_at  timestamptz null;

create table if not exists league_season_snapshots (
  id            uuid        primary key default gen_random_uuid(),
  event_id      uuid        not null,
  season_number integer     not null,
  started_at    timestamptz null,
  ended_at      timestamptz not null default now(),
  standings     jsonb       not null default '[]'::jsonb,
  created_at    timestamptz not null default now(),
  unique (event_id, season_number)
);

create index if not exists league_season_snapshots_event_idx
  on league_season_snapshots (event_id, season_number desc);
