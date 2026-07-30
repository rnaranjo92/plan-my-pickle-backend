-- Which detected player (PB Vision avatar_id, a per-video 0..3 index) is the
-- student, so we can pull THEIR per-player stats out of the analysis. Per-job,
-- because avatar_id is positional per video, not a stable person id.
alter table coaching_pbvision_jobs
  add column if not exists tagged_avatar_id smallint;
