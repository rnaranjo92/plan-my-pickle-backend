-- League video feed: clips posted by league members. Reuses the public
-- `match-videos` Storage bucket for the files (public read → plain-URL playback,
-- own-folder authenticated write), so no new bucket is needed — this table just
-- records the posted URLs. Safe to ship before it runs (feature stays dark).
create table if not exists league_videos (
  id            uuid primary key default gen_random_uuid(),
  league_id     uuid not null references leagues(id) on delete cascade,
  uploaded_by   uuid,
  uploader_name text,
  video_url     text not null,
  title         text,
  created_at    timestamptz not null default now()
);
create index if not exists league_videos_league_idx
  on league_videos(league_id, created_at desc);
