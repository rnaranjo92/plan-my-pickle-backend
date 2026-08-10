-- Grant Premium to a single account by EMAIL.
--
-- Premium is stored as pmp_profiles.premium (a boolean keyed by user_id).
-- Service.IsPremium() reads it when SUBSCRIPTIONS_ENABLED=true. While that env
-- flag is OFF (the current default) EVERYONE is already Premium, so this only
-- takes effect once the paid plan is switched on — setting it now is harmless
-- and future-proofs the account.
--
-- HOW TO RUN: paste into the Supabase dashboard → SQL Editor → Run.
-- To grant someone else later, change the email in BOTH places below.
--
-- Note: the account must have SIGNED UP already (exist in auth.users). If the
-- verify query at the bottom returns 0 rows, they haven't created an account
-- with that email yet.

-- Columns are added defensively so this runs even on a fresh DB (idempotent).
alter table pmp_profiles
  add column if not exists premium boolean not null default false;
alter table pmp_profiles
  add column if not exists subscription_status text;

-- Upsert (rows are created lazily, so the user may not have a profile row yet).
with target as (
  select id as user_id
  from auth.users
  where lower(email) = lower('Jgallanosa@lt.life')
)
insert into pmp_profiles (user_id, premium, subscription_status)
select user_id, true, 'comped' from target
on conflict (user_id) do update
  set premium = true,
      subscription_status = 'comped';

-- Verify — should show one row with premium = true.
select u.email, p.premium, p.subscription_status
from pmp_profiles p
join auth.users u on u.id = p.user_id
where lower(u.email) = lower('Jgallanosa@lt.life');
