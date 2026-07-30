-- Coaching Marketplace (Phase C/D): a player's ENROLLMENT in a class. Free
-- classes are paid=true on insert; paid classes carry amount_cents + charge_at
-- (starts_at − 1h) so a later reconciler can charge off-session ~1h before class
-- (Phase D). One active enrollment per (class, user). FK cascades with the class.
-- Guarded by columnReady, so deploying before this runs is safe.
create table if not exists coaching_enrollments (
  id            uuid primary key default gen_random_uuid(),
  class_id      uuid not null references coaching_classes (id) on delete cascade,
  user_id       uuid not null,
  name          text,
  email         text,
  status        text not null default 'enrolled',  -- enrolled | canceled
  amount_cents  integer not null default 0,
  paid          boolean not null default false,
  charge_at     timestamptz,           -- when to charge (paid classes): starts_at − 1h
  payment_ref   text,                  -- Stripe id (Phase D)
  created_at    timestamptz not null default now(),
  unique (class_id, user_id)
);
create index if not exists coaching_enrollments_class_idx
  on coaching_enrollments (class_id);
create index if not exists coaching_enrollments_user_idx
  on coaching_enrollments (user_id);
alter table coaching_enrollments enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
