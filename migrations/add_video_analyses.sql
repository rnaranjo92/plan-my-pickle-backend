-- Paid "Match Video Analysis" (PB Vision partner integration).
-- A player uploads a match video, pays a one-time fee, we submit it to PB Vision,
-- and the completion webhook fills in the hosted report link + insights/stats.
-- The backend references this table only after a columnReady probe confirms it
-- exists, so deploying the code before this migration runs is safe (the feature
-- simply stays dark).
create table if not exists video_analyses (
  id                uuid primary key default gen_random_uuid(),
  user_id           uuid not null,
  video_url         text not null,          -- public URL PB Vision downloads from
  name              text,                   -- optional game title
  court             text,                   -- optional court label
  partner_emails    text[],                 -- up to 4 player emails for PB Vision
  status            text not null default 'pending_payment',
                                            -- pending_payment | processing | ready | failed
  pb_vid            text,                   -- PB Vision video id (correlates the webhook)
  report_url        text,                   -- hosted PB Vision report (webpage)
  insights          jsonb,
  stats             jsonb,
  error             text,
  amount_cents      int  not null default 0,
  currency          text not null default 'usd',
  stripe_session_id text,
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now()
);
create index if not exists video_analyses_user_idx  on video_analyses (user_id, created_at desc);
create index if not exists video_analyses_pbvid_idx on video_analyses (pb_vid);
-- Service-role only (the Go backend); no anon/authenticated policy.
alter table video_analyses enable row level security;

-- Storage bucket for the uploaded match videos. Public so PB Vision can download
-- the file by URL (paths are user-namespaced + random, not enumerable). Raise the
-- per-file size limit in the Storage dashboard for full-length match clips.
insert into storage.buckets (id, name, public)
  values ('match-videos', 'match-videos', true)
  on conflict (id) do nothing;

-- Authenticated users may upload only into their own {user_id}/ folder.
drop policy if exists "match-videos own upload" on storage.objects;
create policy "match-videos own upload" on storage.objects
  for insert to authenticated
  with check (bucket_id = 'match-videos' and (storage.foldername(name))[1] = auth.uid()::text);
