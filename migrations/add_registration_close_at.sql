-- Optional explicit self-registration cutoff. NULL = no cutoff (falls back to
-- the event-day close). Enforced on self-registration.
alter table events add column if not exists registration_close_at timestamptz;
