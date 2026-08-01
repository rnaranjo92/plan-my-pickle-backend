-- Idempotency ledger for class-pack credit grants: one row per Stripe
-- PaymentIntent (grant_key), so a redelivered checkout.session.completed webhook
-- can't grant the same pack twice. grantPackCredits claims this row first, then
-- sets applied_at ONLY after the credit add succeeds — so a crash between claim
-- and grant is recoverable (a replay with applied_at NULL completes the grant),
-- and a true replay (applied_at set) is skipped.
create table if not exists coaching_credit_grants (
  id uuid primary key default gen_random_uuid(),
  grant_key text unique not null,
  coach_id uuid,
  user_id uuid,
  credits int,
  applied_at timestamptz,
  created_at timestamptz not null default now()
);
