-- Instructor Mode: a drill ASSIGNED to a roster student (their game plan). The
-- coach picks a drill (or writes an ad-hoc goal) for a coach_students row; the
-- student sees it as a goal on their Progress view and can check it done — or the
-- coach can. Drill fields are SNAPSHOT here (title/skill/goal) so editing/deleting
-- the source drill never changes an assignment. FK cascades with the roster row,
-- exactly like coaching_videos / coaching_feedback. Guarded by columnReady.
create table if not exists coaching_assignments (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students (id) on delete cascade,
  drill_id         uuid,                       -- source drill (nullable: ad-hoc or deleted)
  title            text not null,              -- snapshot
  skill_category   text,                        -- snapshot
  goal             text,                        -- snapshot of the target/instructions
  status           text not null default 'assigned',  -- assigned | in_progress | done
  due_at           timestamptz,
  completed_at     timestamptz,
  completed_by     text,                        -- coach | student (who marked it done)
  assigned_by      uuid,
  created_at       timestamptz not null default now()
);
create index if not exists coaching_assignments_cs_idx
  on coaching_assignments (coach_student_id, created_at desc);
alter table coaching_assignments enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
