-- Feedback-latency loop: track when the coach was nudged about an un-answered
-- student clip, so RemindCoachOfPendingClips fires at most once per clip.
alter table coaching_videos
  add column if not exists coach_reminded_at timestamptz;
