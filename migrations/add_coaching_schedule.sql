-- Instructor Mode: the coach's SCHEDULE. One table for three kinds of entries:
--   'session' — a booked lesson with a roster student (coach_student_id + label)
--   'open'    — availability the coach is offering (open for lessons)
--   'blocked' — time the coach is unavailable (day off, vacation)
-- all_day marks a full-day entry (a blocked/open whole day). Guarded by
-- columnReady, so deploying before this runs is safe (the Schedule tab is empty).
create table if not exists coaching_schedule (
  id               uuid primary key default gen_random_uuid(),
  coach_id         uuid not null,
  kind             text not null,              -- session | open | blocked
  coach_student_id uuid references coach_students (id) on delete set null,
  student_label    text,
  starts_at        timestamptz not null,
  ends_at          timestamptz,
  all_day          boolean not null default false,
  location         text,
  notes            text,
  created_at       timestamptz not null default now()
);
create index if not exists coaching_schedule_coach_idx
  on coaching_schedule (coach_id, starts_at);
alter table coaching_schedule enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
