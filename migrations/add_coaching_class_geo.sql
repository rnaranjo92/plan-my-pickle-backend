-- Per-class coordinates so a class can drop its own pin on the player-facing
-- "Classes near me" map. Filled either from an explicit map-picker pin or by
-- best-effort geocoding the typed location on create/update (mirrors events).
alter table coaching_classes add column if not exists lat double precision;
alter table coaching_classes add column if not exists lng double precision;

-- Distance queries scan future classes that have coordinates.
create index if not exists coaching_classes_geo_idx
  on coaching_classes (starts_at)
  where lat is not null;
