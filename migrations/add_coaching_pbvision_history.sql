-- PB Vision report history: every sync appends a snapshot so a coach/student can
-- see how the AI-generated stats have trended over time (not just the latest).
create table if not exists coaching_pbvision_reports (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students(id) on delete cascade,
  rating           numeric(3,2),
  stats            jsonb not null default '{}'::jsonb,
  synced_at        timestamptz not null default now(),
  created_at       timestamptz not null default now()
);
create index if not exists coaching_pbvision_reports_idx
  on coaching_pbvision_reports (coach_student_id, synced_at desc);

alter table coaching_pbvision_reports enable row level security;
