-- Instructor Mode: a coach's PRIVATE note on a clip. Only the coach of the thread
-- can read or write it; it is never included in the student's view of the thread.
-- Use case: prep/reminders ("work the backhand next session"), progress tracking.
-- One editable note per clip. Guarded by columnReady, so safe to deploy before
-- this runs (the note UI just stays hidden until the column exists).
create table if not exists coaching_notes (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students (id) on delete cascade,
  video_id         uuid not null references coaching_videos (id) on delete cascade,
  body             text not null,
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now()
);
create unique index if not exists coaching_notes_video_idx on coaching_notes (video_id);
alter table coaching_notes enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
