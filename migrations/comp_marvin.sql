-- Comp Marvin (motofreak26@hotmail.com) — beta tester who keeps finding real
-- bugs on live league nights.
--
-- NOTE: he can ALREADY generate posters today. The studio only requires being
-- signed in, and event posters only require owning the event — generation is
-- free for everyone during founding access. What this grant does is make sure
-- that stays true for him: `comped` is the column BOTH IsPremium and
-- ClubPlanActive read, and posterAllowed is the future fence that will learn
-- about metering — a comped account passes whatever gets built there.
-- (Same mechanism as comp_founders.sql; Stripe never touches `comped`, so the
-- reconciler can't revoke it when billing goes live.)
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent.
-- He must already have an account with this email; if the verify below returns
-- 0 rows, he hasn't signed up with it.

alter table pmp_profiles add column if not exists comped boolean not null default false;
alter table pmp_profiles add column if not exists comp_reason text;
alter table pmp_profiles add column if not exists comped_at timestamptz;
alter table pmp_profiles add column if not exists comped_by text;

with target as (
  select id as user_id
  from auth.users
  where lower(email) = lower('motofreak26@hotmail.com')
)
insert into pmp_profiles (user_id, comped, comp_reason, comped_at, comped_by)
select user_id, true, 'beta tester (Marvin)', now(), 'kim' from target
on conflict (user_id) do update
  set comped      = true,
      comp_reason = coalesce(pmp_profiles.comp_reason, 'beta tester (Marvin)'),
      comped_at   = coalesce(pmp_profiles.comped_at, now()),
      comped_by   = 'kim';

-- Verify — expect 1 row, comped = true.
select u.email, p.comped, p.comp_reason
from pmp_profiles p
join auth.users u on u.id = p.user_id
where lower(u.email) = lower('motofreak26@hotmail.com');
