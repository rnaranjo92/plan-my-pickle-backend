-- push_subscriptions: one row per DEVICE that can receive a push.
--
-- WHY: targeted sends used to resolve only through OneSignal's
-- include_aliases.external_id, which depends on its identity model being
-- intact. It isn't always — a login-user 409 against a user that already holds
-- the external_id pauses the SDK's op queue, leaving a live subscription owned
-- by no addressable user. The send is then ACCEPTED and delivered to nobody,
-- because the alias DID resolve: to a user with zero devices. No error at
-- either end. Addressing the device by its subscription id skips all of that.
--
-- WHY THIS FILE EXISTS NOW: the Go side has always been gated on
-- pushSubsReady() — `columnReady('push_subscriptions','subscription_id')` — so
-- it degrades silently to the alias path until this table exists. The migration
-- it was waiting for was only ever referenced in a comment, never written. If
-- that is the state of the database, then RecordPushSubscription has been
-- discarding every id it was given and EVERY push in the product has been going
-- out by alias, which is the exact failure the device path was built to escape.
--
-- Run the verification at the bottom first if you want to know which it was.
-- Creating it is idempotent and safe on a live database either way.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run.

create table if not exists push_subscriptions (
  -- OneSignal's subscription id. The PRIMARY KEY, because the upsert conflicts
  -- on it: a device that re-registers must UPDATE its row, not add another.
  -- Keying on user_id instead would either cap a person at one device or
  -- accumulate a row per sign-in — and duplicate rows mean one court call
  -- arriving twice.
  subscription_id text primary key,
  user_id         text not null,
  platform        text,
  -- Refreshed on every sign-in and token refresh, which is what makes it a
  -- liveness signal: sends ignore rows older than 45 days and fall back to the
  -- alias, because a dead-but-existing subscription is never reported invalid
  -- and would otherwise silence that user's fallback permanently.
  updated_at      timestamptz not null default now()
);

-- The send path reads by user, filtered on freshness, so index both.
create index if not exists push_subscriptions_user_idx
  on push_subscriptions (user_id, updated_at desc);

-- Never touched by an end user's token — the backend writes it as the service
-- role — but RLS on means an anon key that reaches it can't read the mapping of
-- accounts to devices.
alter table push_subscriptions enable row level security;

-- VERIFY. Did the table already exist, and is anything in it?
--
--   select count(*) as devices,
--          count(*) filter (where updated_at > now() - interval '45 days')
--            as fresh_devices
--   from push_subscriptions;
--
-- Your own device, after opening the app once on the phone:
--
--   select subscription_id, platform, updated_at
--   from push_subscriptions
--   where user_id = '<your supabase user id>'
--   order by updated_at desc;
--
-- No row = the app hasn't recorded this device yet (open it while signed in, or
-- tap the Test-tab push button). A row = the next send addresses the device
-- directly, and Railway will log `push via devices: ... recipients=1`.
