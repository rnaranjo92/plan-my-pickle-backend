-- One PB Vision analysis (a doubles match = up to 4 players) can cover up to 4
-- students: the coach assigns each detected player (avatar_id) to one of their
-- students' threads. Each student then sees only their own player's stats.
-- A player maps to at most one student, and a student to at most one player,
-- per analysis (both enforced by unique constraints => natural max of 4).
create table if not exists coaching_pbvision_assignments (
  id               uuid primary key default gen_random_uuid(),
  job_id           uuid not null references coaching_pbvision_jobs(id) on delete cascade,
  avatar_id        smallint not null,
  coach_student_id uuid not null references coach_students(id) on delete cascade,
  created_at       timestamptz not null default now(),
  unique (job_id, avatar_id),
  unique (job_id, coach_student_id)
);
create index if not exists coaching_pbvision_assignments_student_idx
  on coaching_pbvision_assignments (coach_student_id);
create index if not exists coaching_pbvision_assignments_job_idx
  on coaching_pbvision_assignments (job_id);
alter table coaching_pbvision_assignments enable row level security;
