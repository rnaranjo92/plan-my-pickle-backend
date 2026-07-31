-- Reusable program templates: a coach saves a multi-week plan once and applies
-- it to many students. Weeks carry focus + drills (no per-student due dates).
create table if not exists coach_program_templates (
  id         uuid primary key default gen_random_uuid(),
  coach_id   uuid not null,
  title      text not null,
  weeks      jsonb not null default '[]'::jsonb,
  created_at timestamptz not null default now()
);

create index if not exists coach_program_templates_coach_idx
  on coach_program_templates (coach_id, created_at desc);
