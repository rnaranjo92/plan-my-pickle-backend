-- Comp Austen (asveom@lt.life) — instructor, coaching mentor, and AI-poster
-- tester.
--
-- Why this exists as a migration: the notes say he was comped by hand on
-- 2026-08-17, but no script recorded it, so nothing in the repo can prove it
-- and nothing would restore it. `comped` is the column BOTH IsPremium and
-- ClubPlanActive read, and posterCompedUnlimited bypasses poster metering
-- entirely — so this grant is what makes poster generation free for him now
-- that POSTER_CREDITS_ENABLED is on and a normal account starts at zero.
--
-- Stripe never touches `comped`, so the subscription reconciler can't revoke
-- it when billing goes live. Same mechanism as comp_marvin.sql and
-- comp_founders.sql.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent — safe
-- to run again even if the hand grant already happened. He must already have an
-- account with this email; if the verify below returns 0 rows, he signed up
-- with a different one.

alter table pmp_profiles add column if not exists comped boolean not null default false;
alter table pmp_profiles add column if not exists comp_reason text;
alter table pmp_profiles add column if not exists comped_at timestamptz;
alter table pmp_profiles add column if not exists comped_by text;

with target as (
  select id as user_id
  from auth.users
  where lower(email) = lower('asveom@lt.life')
)
insert into pmp_profiles (user_id, comped, comp_reason, comped_at, comped_by)
select user_id, true, 'instructor + AI poster tester (Austen)', now(), 'kim'
from target
on conflict (user_id) do update
  set comped      = true,
      comp_reason = coalesce(pmp_profiles.comp_reason,
                             'instructor + AI poster tester (Austen)'),
      comped_at   = coalesce(pmp_profiles.comped_at, now()),
      comped_by   = coalesce(pmp_profiles.comped_by, 'kim');

-- Verify (expect 1 row, comped = true):
--   select p.user_id, p.comped, p.comp_reason, p.comped_at
--   from pmp_profiles p
--   join auth.users u on u.id = p.user_id
--   where lower(u.email) = lower('asveom@lt.life');
