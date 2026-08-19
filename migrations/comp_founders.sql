-- Comp the two founder accounts (Kim + Krizhia) so they get Premium AND the
-- Club tier free, permanently — and, critically, the grant SURVIVES billing
-- being switched on.
--
--   krizhia_roxas29@yahoo.com
--   rolando.naranjo0420@gmail.com
--
-- WHY `comped` AND NOT `premium`: Stripe OWNS the `premium` column. The moment
-- SUBSCRIPTIONS_ENABLED=true, the hourly reconciler asks Stripe "who is actually
-- subscribed?" and rewrites `premium` from the answer — which would REVOKE any
-- hand-set premium row, since these accounts aren't paying. `comped` is the
-- column Stripe never touches. Service.IsPremium() and ClubPlanActive() BOTH
-- read it, so this single flag grants both tiers at once and nothing can quietly
-- take it away.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent — safe
-- to run more than once. The accounts must already exist in auth.users (they've
-- signed up). If the verify query at the bottom returns fewer than 2 rows, that
-- email hasn't registered an account yet.

-- Columns added defensively so this runs even before add_comped_access.sql.
alter table pmp_profiles add column if not exists comped boolean not null default false;
alter table pmp_profiles add column if not exists comp_reason text;
alter table pmp_profiles add column if not exists comped_at timestamptz;
alter table pmp_profiles add column if not exists comped_by text;

with target as (
  select id as user_id
  from auth.users
  where lower(email) in (
    lower('krizhia_roxas29@yahoo.com'),
    lower('rolando.naranjo0420@gmail.com')
  )
)
insert into pmp_profiles (user_id, comped, comp_reason, comped_at, comped_by)
select user_id, true, 'founder', now(), 'founder' from target
on conflict (user_id) do update
  set comped      = true,
      comp_reason = coalesce(pmp_profiles.comp_reason, 'founder'),
      comped_at   = coalesce(pmp_profiles.comped_at, now()),
      comped_by   = 'founder';

-- Verify — expect 2 rows, comped = true.
select u.email, p.comped, p.comp_reason
from pmp_profiles p
join auth.users u on u.id = p.user_id
where lower(u.email) in (
  lower('krizhia_roxas29@yahoo.com'),
  lower('rolando.naranjo0420@gmail.com')
);
