-- Instructor Mode: a LIST of shared notes the coach posts for a student (each
-- with a title + timestamp), replacing the single coach_students.shared_note
-- text field. The student sees all of them. A note is only editable/deletable
-- within 24h of posting (enforced in the Go service). FK cascades with the
-- roster row, like the other coaching tables. Guarded by columnReady.
create table if not exists coaching_shared_notes (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students (id) on delete cascade,
  title            text,
  body             text not null,
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now()
);
create index if not exists coaching_shared_notes_cs_idx
  on coaching_shared_notes (coach_student_id, created_at desc);
alter table coaching_shared_notes enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
