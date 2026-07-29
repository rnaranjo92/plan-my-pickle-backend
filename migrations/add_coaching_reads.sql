-- Instructor Mode Phase 1.5: coaching read-receipts / unread indicators.
-- Tracks the last time each participant viewed a thread, and stamps each thread's
-- last activity, so the roster (coach) and "My Coaching" list (student) can flag
-- threads with something new. Backend probes coaching_reads.id / the new column
-- via columnReady, so deploying before this runs is safe (unread just stays off).

-- When a thread last got a new clip or comment. Bumped by the backend on each
-- upload/feedback. Existing rows get now() on add.
alter table coach_students
  add column if not exists last_activity_at timestamptz not null default now();

-- One row per (viewer, thread): when that viewer last opened the thread. The
-- viewer is either the coach or the student. A thread is "unread" for a viewer
-- when last_activity_at > their last_seen_at (or they have no row yet).
create table if not exists coaching_reads (
  id               uuid primary key default gen_random_uuid(),
  user_id          uuid not null,
  coach_student_id uuid not null references coach_students (id) on delete cascade,
  last_seen_at     timestamptz not null default now()
);
create unique index if not exists coaching_reads_user_thread_idx
  on coaching_reads (user_id, coach_student_id);
alter table coaching_reads enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
