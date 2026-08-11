-- Coach + Premium for ASveom@lt.life, using their known account id directly.
-- user_id: 28f2f7ea-384c-4982-9108-3879dae9cfb1
--
-- Using the UID avoids depending on a subquery against auth.users, which the
-- SQL editor may return 0 rows for depending on schema access.
--
-- Coach access is matched BY EMAIL (Service.IsInstructor), so the email MUST be
-- stored lowercase — a stored "ASveom@lt.life" would silently never match and
-- they'd see no Coach tab with no error. user_id is informational but worth
-- setting. Premium is keyed by user_id (Service.IsPremium reads
-- pmp_profiles.premium) and only matters once SUBSCRIPTIONS_ENABLED is on.
--
-- Safe to re-run. HOW TO RUN: Supabase dashboard -> SQL Editor -> paste -> Run.
-- Allow ~2 min before testing: the backend negative-caches a missing table for
-- two minutes, so it may briefly still think `instructors` is absent.

-- 0) Coach allowlist table (had never been run in prod, which is why
--    "Manage coaches" silently did nothing) --------------------------------
create table if not exists instructors (
  id         uuid primary key default gen_random_uuid(),
  email      text not null,
  name       text,
  user_id    uuid,
  created_at timestamptz not null default now()
);
create unique index if not exists instructors_email_idx on instructors (lower(email));
alter table instructors enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.

-- 1) COACH ACCESS -------------------------------------------------------------
insert into instructors (email, name, user_id)
select lower('ASveom@lt.life'), null, '28f2f7ea-384c-4982-9108-3879dae9cfb1'::uuid
where not exists (
  select 1 from instructors where lower(email) = lower('ASveom@lt.life')
);

-- If a row already existed without the account link, attach it now.
update instructors
   set user_id = '28f2f7ea-384c-4982-9108-3879dae9cfb1'::uuid
 where lower(email) = lower('ASveom@lt.life')
   and user_id is null;

-- 2) PREMIUM ------------------------------------------------------------------
alter table pmp_profiles
  add column if not exists premium boolean not null default false;
alter table pmp_profiles
  add column if not exists subscription_status text;

insert into pmp_profiles (user_id, premium, subscription_status)
values ('28f2f7ea-384c-4982-9108-3879dae9cfb1'::uuid, true, 'comped')
on conflict (user_id) do update
  set premium = true,
      subscription_status = 'comped';

-- 3) VERIFY — expect one row from each ----------------------------------------
select 'coach' as grant, email, user_id
from instructors
where lower(email) = lower('ASveom@lt.life');

select 'premium' as grant, user_id, premium, subscription_status
from pmp_profiles
where user_id = '28f2f7ea-384c-4982-9108-3879dae9cfb1'::uuid;
