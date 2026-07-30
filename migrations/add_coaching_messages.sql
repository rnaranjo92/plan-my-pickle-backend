-- Free-form chat on a coaching thread (distinct from clip-scoped feedback): a
-- coach and student can message directly. Keyed to the coach_students thread.
create table if not exists coaching_messages (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students(id) on delete cascade,
  sender_id        text not null,
  sender_role      text not null, -- coach | student
  body             text not null,
  created_at       timestamptz not null default now()
);
create index if not exists coaching_messages_thread_idx
  on coaching_messages (coach_student_id, created_at);

alter table coaching_messages enable row level security;
-- Service-role only; the Go layer authorizes by thread membership.
