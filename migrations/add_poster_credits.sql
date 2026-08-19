-- AI poster credits: $2.99 buys 5 generations; Club subscribers get 5 included
-- every calendar month (Pacific-anchored, so "this month" is the organizer's).
--
-- NOTHING CHANGES WHEN YOU RUN THIS. Metering is off until
-- POSTER_CREDITS_ENABLED=true is set in Railway — until then generation stays
-- free for everyone and these columns just sit at zero. Run this FIRST, flip the
-- flag when you're ready to charge.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent.

-- Balance bought with money. Club's monthly allowance is tracked separately so
-- a paid credit is never silently consumed while free ones remain.
alter table pmp_profiles
  add column if not exists poster_credits integer not null default 0;
-- How many of this month's Club allowance have been used, and WHICH month that
-- count belongs to. A stale period means the allowance has rolled over.
alter table pmp_profiles
  add column if not exists poster_club_used integer not null default 0;
alter table pmp_profiles
  add column if not exists poster_club_period text;

-- Idempotency for Stripe: one row per PaymentIntent. Stripe redelivers webhooks,
-- and the unique index is what stops a redelivery handing out a second pack.
create table if not exists poster_credit_grants (
  id          uuid primary key default gen_random_uuid(),
  payment_ref text not null,
  user_id     text not null,
  credits     integer not null,
  created_at  timestamptz not null default now()
);
create unique index if not exists poster_credit_grants_ref_idx
  on poster_credit_grants (payment_ref);
alter table poster_credit_grants enable row level security;

-- Verify:
--   select count(*) from poster_credit_grants;
--   select poster_credits, poster_club_used, poster_club_period
--     from pmp_profiles limit 1;
