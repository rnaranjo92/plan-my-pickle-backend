-- PB Vision analytics per coaching thread: the AI game-report stats (shot
-- quality, speeds, kitchen arrival, shot mix, etc.) a coach syncs for a student.
-- Stored as a flexible jsonb blob since PB Vision's payload is rich/evolving; a
-- top-level rating is denormalized for quick display. One row per thread.
create table if not exists coaching_pbvision (
  coach_student_id uuid primary key
    references coach_students(id) on delete cascade,
  rating         numeric(3,2),
  stats          jsonb not null default '{}'::jsonb,
  last_synced_at timestamptz,
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now()
);

alter table coaching_pbvision enable row level security;
-- Service-role only (like every coaching table); the Go layer authorizes by
-- thread membership. No anon/authenticated policy on purpose.
