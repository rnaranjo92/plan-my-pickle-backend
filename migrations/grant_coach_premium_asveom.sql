-- Grant ASveom@lt.life BOTH coach access and Premium.
--
-- NOTE: block 0 creates the `instructors` table, which had NEVER been run in
-- production. Until now coach access was effectively owners-only (the two
-- founding-owner emails are coaches in code); the "Manage coaches" screen and
-- POST /admin/instructors silently no-opped because the backend probes for this
-- table via columnReady and falls back when it's missing. Running this once
-- turns that whole feature on.
--
-- Two independent grants, stored in different places:
--   1. COACH ACCESS -> a row in `instructors`, matched by EMAIL by
--      Service.IsInstructor(). Unlocks the "Coach" tab / instructor mode.
--      The email MUST be stored LOWERCASE: IsInstructor lowercases the caller's
--      email and does an exact match, so "ASveom@lt.life" would never match.
--   2. PREMIUM -> pmp_profiles.premium, read by Service.IsPremium().
--
-- The coach half works even before they sign up (it matches on email). The
-- Premium half needs an auth.users row — if they're new, re-run block 2 after
-- they create their account.
--
-- HOW TO RUN: Supabase dashboard -> SQL Editor -> paste -> Run.

-- 0) CREATE THE COACH ALLOWLIST TABLE (migrations/add_instructors.sql) ---------
create table if not exists instructors (
  id         uuid primary key default gen_random_uuid(),
  email      text not null,
  name       text,
  user_id    uuid,               -- resolved account id when known (informational)
  created_at timestamptz not null default now()
);
create unique index if not exists instructors_email_idx on instructors (lower(email));
alter table instructors enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.

-- 1) COACH ACCESS -------------------------------------------------------------
insert into instructors (email, name, user_id)
select lower('ASveom@lt.life'),
       null,
       (select id from auth.users where lower(email) = lower('ASveom@lt.life'))
where not exists (
  select 1 from instructors where lower(email) = lower('ASveom@lt.life')
);

-- 2) PREMIUM ------------------------------------------------------------------
alter table pmp_profiles
  add column if not exists premium boolean not null default false;
alter table pmp_profiles
  add column if not exists subscription_status text;

with target as (
  select id as user_id
  from auth.users
  where lower(email) = lower('ASveom@lt.life')
)
insert into pmp_profiles (user_id, premium, subscription_status)
select user_id, true, 'comped' from target
on conflict (user_id) do update
  set premium = true,
      subscription_status = 'comped';

-- 3) VERIFY -------------------------------------------------------------------
-- Coach row: expect 1 row, email stored lowercase.
select 'coach' as grant, email, user_id is not null as linked_to_account
from instructors
where lower(email) = lower('ASveom@lt.life');

-- Premium row: expect 1 row with premium = true. ZERO rows just means they
-- haven't signed up yet — the coach grant still works; re-run block 2 later.
select 'premium' as grant, u.email, p.premium, p.subscription_status
from pmp_profiles p
join auth.users u on u.id = p.user_id
where lower(u.email) = lower('ASveom@lt.life');
