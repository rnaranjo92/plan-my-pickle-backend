-- Club dues: a season fee, and who has paid it.
--
-- Kim's decisions (2026-08-17): a ONE-OFF season fee rather than a recurring
-- subscription, and paying is STATUS ONLY — it never blocks anyone from
-- registering for anything. Same rule as the coach cap and the season archive:
-- nobody loses access to the thing they turned up for because of a billing
-- state.
--
-- A PERIOD rather than a flag on the club, because "have you paid" is only ever
-- meaningful about a season. Next year the club opens a new period and last
-- year's record stays intact — which is the whole point of being the system of
-- record rather than a spreadsheet somebody overwrites each spring.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run. Idempotent.

create table if not exists club_dues_periods (
  id          uuid primary key default gen_random_uuid(),
  club_id     uuid not null references clubs(id) on delete cascade,
  -- What the club calls it: "2026 Season", "Autumn term", "Membership 2026".
  name        text not null,
  amount_cents integer not null check (amount_cents >= 0),
  currency    text not null default 'usd',
  created_at  timestamptz not null default now(),
  -- Closed periods stay readable forever; only one is open at a time.
  closed_at   timestamptz
);

create index if not exists club_dues_periods_club_idx
  on club_dues_periods (club_id, created_at desc);

create table if not exists club_dues_payments (
  period_id  uuid not null references club_dues_periods(id) on delete cascade,
  user_id    text not null,
  -- How it was collected. Most small clubs take cash, Zelle or a transfer, so
  -- this records reality rather than pretending every payment came through the
  -- app. 'stripe' is set by the payment webhook when that path is added.
  method     text not null default 'manual',
  amount_cents integer,
  note       text,
  -- Who recorded it, for a club with more than one person taking money.
  recorded_by text,
  paid_at    timestamptz not null default now(),
  primary key (period_id, user_id)
);

create index if not exists club_dues_payments_user_idx
  on club_dues_payments (user_id);

alter table club_dues_periods enable row level security;
alter table club_dues_payments enable row level security;

-- Verify:
--   select count(*) from club_dues_periods;
--   select count(*) from club_dues_payments;
