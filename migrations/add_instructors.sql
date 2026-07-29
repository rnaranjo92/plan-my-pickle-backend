-- Instructor Mode: the coach allowlist. A user is a coach if their account email
-- is in this table (or is one of the two founding-owner emails, which are always
-- coaches in code). Add a row here — or use the owner-only "Manage coaches" screen
-- / POST /admin/instructors — to grant someone the coach view. The backend probes
-- this table via columnReady, so deploying before it runs is safe (falls back to
-- owners-only).
create table if not exists instructors (
  id         uuid primary key default gen_random_uuid(),
  email      text not null,
  name       text,
  user_id    uuid,               -- resolved account id when known (informational)
  created_at timestamptz not null default now()
);
create unique index if not exists instructors_email_idx on instructors (lower(email));
alter table instructors enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
