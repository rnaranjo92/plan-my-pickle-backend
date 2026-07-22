-- SMS metering ledger — one row per event owner per calendar month, counting
-- A2P SMS segments sent on their behalf. The backend gates sends on this so a
-- flat "unlimited SMS" plan can never be eaten by carrier fees (which vary ~20×
-- across countries); once an owner passes SMS_MONTHLY_ALLOWANCE, sends degrade
-- to push for the rest of the month. Inert until SMS_MONTHLY_ALLOWANCE is set.
create table if not exists sms_usage (
  owner_id   text        not null,
  period     text        not null,           -- UTC calendar month, 'YYYY-MM'
  sent       integer     not null default 0, -- SMS segments sent this period
  updated_at timestamptz not null default now(),
  primary key (owner_id, period)
);

-- Fast "usage this month" lookups on the hot path (checked once per send batch).
create index if not exists sms_usage_period_idx on sms_usage (period);
