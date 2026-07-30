-- PB Vision auto-import scaffolding: tag where a coaching clip came from, link a
-- PB Vision analysis job to a thread, and store a student's PB Vision athlete id
-- for attribution. Highlights/stats ingestion is wired from the completion webhook.
alter table coaching_videos add column if not exists source       text not null default 'upload'; -- upload | pbvision
alter table coaching_videos add column if not exists external_ref  text;                            -- PB Vision clip id (dedupe)
alter table coach_students  add column if not exists pbvision_player_id text;                        -- linked PB Vision athlete

-- One PB Vision analysis submission, correlated to a thread by the returned vid.
create table if not exists coaching_pbvision_jobs (
  id               uuid primary key default gen_random_uuid(),
  vid              text not null unique,          -- PB Vision video id
  coach_student_id uuid not null references coach_students(id) on delete cascade,
  status           text not null default 'processing', -- processing | ready | failed
  report_url       text,                          -- PB Vision hosted report page
  insights         jsonb,                         -- raw insights payload (highlights/players)
  stats            jsonb,                         -- raw stats payload
  error            text,
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now()
);
create index if not exists coaching_pbvision_jobs_vid_idx on coaching_pbvision_jobs (vid);
create index if not exists coaching_pbvision_jobs_thread_idx on coaching_pbvision_jobs (coach_student_id);
alter table coaching_pbvision_jobs enable row level security;
