-- Multi-week training programs: a coach assigns a structured plan to a student
-- (e.g. "8-week third-shot mastery"). Weeks are a jsonb array of {focus, done}
-- so the whole plan lives in one row. One active program per thread (latest).
create table if not exists coaching_programs (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students(id) on delete cascade,
  title            text not null,
  weeks            jsonb not null default '[]'::jsonb,
  active           boolean not null default true,
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now()
);
create index if not exists coaching_programs_thread_idx
  on coaching_programs (coach_student_id, created_at desc);

alter table coaching_programs enable row level security;
