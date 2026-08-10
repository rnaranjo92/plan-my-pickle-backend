-- Grant Premium to michellecruzsd@gmail.com.
--
-- Premium is stored as pmp_profiles.premium (boolean, keyed by user_id) and read
-- by Service.IsPremium() — but ONLY when SUBSCRIPTIONS_ENABLED=true. While that
-- env flag is OFF (the current default) everyone is already treated as Premium,
-- so running this now is harmless and simply future-proofs her account for when
-- paid plans switch on.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run.
--
-- NOTE: she must have SIGNED UP already (exist in auth.users). If the verify
-- query at the bottom returns 0 rows, no account exists with that email yet —
-- check for a typo or a different signup address before assuming it worked.
--
-- SEPARATE from her ladder-only grant: that restriction lives in the BACKEND
-- code (ladderOnlyGrants in internal/api/server.go, mirrored by
-- _ladderOnlyEmails in the app), NOT in the database. This script does not lift
-- it — she stays ladder-only and additionally becomes Premium. If you want her
-- to create tournaments/other league types too, that's a code change + deploy.

-- Defensive column adds so this runs on any DB state (idempotent).
alter table pmp_profiles
  add column if not exists premium boolean not null default false;
alter table pmp_profiles
  add column if not exists subscription_status text;

-- Upsert: profile rows are created lazily, so she may not have one yet.
with target as (
  select id as user_id
  from auth.users
  where lower(email) = lower('michellecruzsd@gmail.com')
)
insert into pmp_profiles (user_id, premium, subscription_status)
select user_id, true, 'comped' from target
on conflict (user_id) do update
  set premium = true,
      subscription_status = 'comped';

-- Verify — expect exactly one row, premium = true, status = comped.
select u.email, p.premium, p.subscription_status
from pmp_profiles p
join auth.users u on u.id = p.user_id
where lower(u.email) = lower('michellecruzsd@gmail.com');
