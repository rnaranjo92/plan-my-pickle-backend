-- Per-division config: placeholder-player count, wave start time, and team (MLP)
-- setup. All nullable + idempotent, so existing divisions are unaffected.
alter table brackets add column if not exists player_count     int;  -- expected players (schedule preview)
alter table brackets add column if not exists start_minutes    int;  -- wave start, minute-of-day (480 = 8am)
alter table brackets add column if not exists team_count       int;  -- MLP: number of teams
alter table brackets add column if not exists players_per_team int;  -- MLP: players per team (4 = 2M+2W)
