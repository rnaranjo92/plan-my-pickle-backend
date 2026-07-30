-- Tie a PB Vision analysis job to the specific coaching clip it was launched
-- from, so the clip can show a "processing / ready / failed" status chip.
alter table coaching_pbvision_jobs
  add column if not exists source_video_id uuid references coaching_videos(id) on delete set null;

create index if not exists coaching_pbvision_jobs_source_video_idx
  on coaching_pbvision_jobs (source_video_id);
