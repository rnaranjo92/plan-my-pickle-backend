-- Event waitlist: people who tried to register after the MaxPlayers cap was hit.
-- Kept separate from registrations so it never affects standings/scheduling/
-- counts until an organizer promotes an entry into a real registration.
create table if not exists event_waitlist (
  id          uuid primary key,
  event_id    uuid not null references events(id) on delete cascade,
  bracket_id  uuid references brackets(id) on delete set null,
  full_name   text not null,
  phone       text,
  email       text,
  user_id     uuid,
  skill_level real,
  sms_consent boolean not null default false,
  created_at  timestamptz not null default now()
);
create index if not exists event_waitlist_event_idx on event_waitlist(event_id);
