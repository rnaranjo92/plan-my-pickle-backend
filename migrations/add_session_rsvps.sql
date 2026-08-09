-- Per-player RSVP for a league's NEXT session ("are you coming Thursday?").
-- Perpetual leagues run as one ongoing event; the RSVP is keyed by the upcoming
-- session DATE (computed from the recurring cadence) so each week is its own poll.
-- The app treats a missing table as "RSVP off" (columnReady guard), so this can
-- be run any time.

create table if not exists session_rsvps (
  id           uuid        primary key default gen_random_uuid(),
  event_id     uuid        not null,
  user_id      uuid        not null,
  session_date date        not null,
  status       text        not null default 'going', -- going | maybe | out
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now(),
  unique (event_id, user_id, session_date)
);

create index if not exists session_rsvps_event_date_idx
  on session_rsvps (event_id, session_date);

-- Organizer opt-in: the RSVP strip only shows when the league toggles it on in
-- the edit form. Default off (opt-in), so enabling the feature is a deliberate
-- per-league choice.
alter table events
  add column if not exists rsvp_enabled boolean not null default false;
