-- Idempotency ledger for class-pack credit grants: one row per Stripe
-- PaymentIntent (grant_key), so a redelivered checkout.session.completed webhook
-- can't grant the same pack twice. grantPackCredits claims this row first.
create table if not exists coaching_credit_grants (
  id uuid primary key default gen_random_uuid(),
  grant_key text unique not null,
  coach_id uuid,
  user_id uuid,
  credits int,
  created_at timestamptz not null default now()
);
