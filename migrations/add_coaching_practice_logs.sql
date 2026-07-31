-- Practice log: students self-log practice between sessions (accountability +
-- streaks). One row per logged practice.
create table if not exists coaching_practice_logs (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students(id) on delete cascade,
  user_id          uuid,
  note             text,
  created_at       timestamptz not null default now()
);

create index if not exists coaching_practice_logs_thread_idx
  on coaching_practice_logs (coach_student_id, created_at desc);
