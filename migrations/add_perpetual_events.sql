-- Perpetual (single-event) recurring leagues: a recurring/"forever" league now
-- runs as ONE ongoing event (the normal tournament interface) instead of
-- spawning a fresh session event every week. That event is flagged `perpetual`:
--   * it does NOT clone (recur_interval_days is set to 0 on adoption),
--   * standings + games accumulate season-long,
--   * check-ins auto-reset each day so every session re-takes attendance.
-- Safe pre-run (defaults false; existing events unaffected until adopted).
alter table events add column if not exists perpetual boolean not null default false;

-- Speeds the ticker's "which events roll over check-ins" sweep.
create index if not exists idx_events_perpetual on events (perpetual) where perpetual;
