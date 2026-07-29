-- Instructor Mode — Phase 1: coach↔student video feedback.
-- A coach (gated to instructor accounts) keeps a small roster of students,
-- uploads short clips into a shared "thread" per student, and exchanges text
-- feedback. Either party can upload a clip or comment; the other party gets a
-- bell + push notification. The roster is addressed by the student's EMAIL so a
-- coach can add someone before their account id is known; student_id is filled
-- in (best-effort at add time, then backfilled the first time the student opens
-- their coaching view). The backend references these tables only after a
-- columnReady probe, so deploying the code before this migration runs is safe
-- (the feature simply stays dark).

-- Roster: one row per coach↔student relationship. This row's id IS the thread id.
create table if not exists coach_students (
  id            uuid primary key default gen_random_uuid(),
  coach_id      uuid not null,                 -- instructor's auth user id
  student_email text not null,                 -- lowercased; the stable address
  student_name  text,                          -- display name the coach typed
  student_id    uuid,                          -- resolved account id (nullable)
  created_at    timestamptz not null default now()
);
-- A coach can't add the same student email twice.
create unique index if not exists coach_students_coach_email_idx
  on coach_students (coach_id, lower(student_email));
create index if not exists coach_students_coach_idx   on coach_students (coach_id, created_at desc);
create index if not exists coach_students_email_idx   on coach_students (lower(student_email));
create index if not exists coach_students_student_idx on coach_students (student_id);
alter table coach_students enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.

-- Clips uploaded into a thread. uploader_role is 'coach' or 'student'.
create table if not exists coaching_videos (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students (id) on delete cascade,
  uploaded_by      uuid not null,              -- auth user id of the uploader
  uploader_role    text not null,              -- coach | student
  video_url        text not null,              -- public URL in coaching-videos bucket
  title            text,
  created_at       timestamptz not null default now()
);
create index if not exists coaching_videos_thread_idx on coaching_videos (coach_student_id, created_at desc);
alter table coaching_videos enable row level security;

-- Text feedback. video_id is nullable so a thread-level note is possible.
create table if not exists coaching_feedback (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students (id) on delete cascade,
  video_id         uuid references coaching_videos (id) on delete cascade,
  author_id        uuid not null,
  author_role      text not null,              -- coach | student
  body             text not null,
  created_at       timestamptz not null default now()
);
create index if not exists coaching_feedback_thread_idx on coaching_feedback (coach_student_id, created_at);
create index if not exists coaching_feedback_video_idx  on coaching_feedback (video_id, created_at);
alter table coaching_feedback enable row level security;

-- Storage bucket for coaching clips. Public so playback works via a plain URL
-- (paths are user-namespaced + random, not enumerable). Move to a private bucket
-- + signed URLs in a later phase if student-video privacy needs tightening.
insert into storage.buckets (id, name, public)
  values ('coaching-videos', 'coaching-videos', true)
  on conflict (id) do nothing;

-- Authenticated users may upload only into their own {user_id}/ folder.
drop policy if exists "coaching-videos own upload" on storage.objects;
create policy "coaching-videos own upload" on storage.objects
  for insert to authenticated
  with check (bucket_id = 'coaching-videos' and (storage.foldername(name))[1] = auth.uid()::text);
