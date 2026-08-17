-- Switch event 8de1bd8c-79e2-41b5-be72-7b775693264d from round_robin to
-- pools -> playoff (medal bracket).
--
-- Why SQL: UpdateEvent (internal/service/service.go) deliberately never writes
-- tournament_format -- the structural format is treated as fixed once the draw
-- exists -- so the Edit-event screen can't do this. Run in Supabase -> SQL Editor.
--
-- What changes, in practice:
--   * The Game tab gains the "Build playoff" button (the Flutter app gates it on
--     tournamentFormat == 'pools_playoff'). Once every pool match in a division
--     is scored, that seeds a medal bracket from the standings: SF1 = 1v4,
--     SF2 = 2v3, winners play gold, losers play bronze.
--   * Existing pool matches are NOT touched or regenerated. With 6 teams per
--     division the pool split is a no-op anyway (splitPools keeps one pool below
--     8 teams), so the round-robin games you already have ARE the pool round --
--     the playoff just gets stacked on top.
--   * A division needs at least 4 teams to build a playoff. Both of yours have 6.
--
-- Idempotent: re-running is a no-op.

-- 1. BEFORE ------------------------------------------------------------------
select 'before' as when, id, name, status, format, partner_mode,
       tournament_format, num_courts, min_pool_rounds, max_pool_rounds
from events
where id = '8de1bd8c-79e2-41b5-be72-7b775693264d';

-- 2. THE FLIP ----------------------------------------------------------------
update events
set tournament_format = 'pools_playoff'
where id = '8de1bd8c-79e2-41b5-be72-7b775693264d'
  and tournament_format is distinct from 'pools_playoff';

-- 3. AFTER + sanity check ----------------------------------------------------
select 'after' as when, id, name, status, format, partner_mode, tournament_format
from events
where id = '8de1bd8c-79e2-41b5-be72-7b775693264d';

-- Per-division readiness: teams registered, pool games built, pool games still
-- open. "Build playoff" refuses until open_pool_games = 0 for that division.
-- Only APPROVED registrations feed the draw (bracketRegs filters approved=is.true),
-- so `teams` counts those -- pending_regs would be excluded from the seeding.
select b.name                                              as division,
       count(distinct r.id) filter (where r.approved is true)     as approved_regs,
       count(distinct r.id) filter (where r.approved is true) / 2 as teams,
       count(distinct r.id) filter (where r.approved is not true) as pending_regs,
       count(distinct m.id) filter (where m.stage = 'pool') as pool_games,
       count(distinct m.id) filter (
         where m.stage = 'pool' and m.status <> 'completed') as open_pool_games,
       count(distinct m.id) filter (where m.stage = 'playoff') as playoff_games
from brackets b
left join registrations r on r.bracket_id = b.id
left join matches m       on m.bracket_id = b.id
where b.event_id = '8de1bd8c-79e2-41b5-be72-7b775693264d'
group by b.name
order by b.name;
