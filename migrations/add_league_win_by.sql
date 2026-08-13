-- League-level win margin.
--
-- Every league session was born win-by-2: SetLeagueSchedule passed a hardcoded
-- WinBy: 2 to CreateEvent, so a league organizer who wanted "first to 11 wins,
-- 11-10 is final" had to edit each generated session by hand — and any session
-- created later silently reverted to 2.
--
-- NULL means "not chosen", which reads as the app-wide default of 2. Only 1 or 2
-- are ever written. The value is a DEFAULT for sessions, not a live reference:
-- each event keeps its own win_by (finished games are scored against the rule in
-- force at the time), so changing this never rewrites history.
alter table leagues
  add column if not exists win_by smallint;

comment on column leagues.win_by is
  'Default win margin (1 or 2) for sessions this league creates. NULL = 2.';
