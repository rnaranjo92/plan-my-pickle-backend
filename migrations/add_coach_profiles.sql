-- Coaching Marketplace (player side): a coach's PUBLIC discovery profile. A coach
-- opts in (listed) and sets a bio, rate, skills, and a location; players find
-- listed coaches nearest to them by GPS distance. Keyed by the coach's account
-- (user_id). Guarded by columnReady, so deploying before this runs is safe (the
-- "Find a coach" list is just empty and the profile editor stays hidden).
create table if not exists coach_profiles (
  user_id           uuid primary key,
  name              text,
  listed            boolean not null default false,
  bio               text,
  city              text,
  lat               double precision,
  lng               double precision,
  hourly_rate_cents integer,
  skills            text,
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now()
);
create index if not exists coach_profiles_listed_idx
  on coach_profiles (listed);
alter table coach_profiles enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
