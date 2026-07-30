-- Coaching Marketplace (Phase B): group CLASSES a coach offers that players can
-- enroll in. price_cents 0 = free. capacity is the seat cap. Discovery/enroll
-- (enrollments) come in a later migration. Guarded by columnReady, so deploying
-- before this runs is safe (the classes list just stays empty).
create table if not exists coaching_classes (
  id          uuid primary key default gen_random_uuid(),
  coach_id    uuid not null,              -- the coach's account (user_id)
  title       text not null,
  description text,
  starts_at   timestamptz not null,
  ends_at     timestamptz,
  location    text,
  capacity    integer not null default 0, -- 0 = unlimited
  price_cents integer not null default 0, -- 0 = free
  created_at  timestamptz not null default now()
);
create index if not exists coaching_classes_coach_idx
  on coaching_classes (coach_id, starts_at);
alter table coaching_classes enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
