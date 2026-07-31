-- Onboarding nudge: track when a joined-but-never-uploaded student was nudged
-- to send their first clip, so RemindNeverUploaded fires at most once.
alter table coach_students
  add column if not exists first_nudge_at timestamptz;
