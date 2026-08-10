-- Multi-coach PB Vision: one analysis (a PB Vision `vid`) can be shared to
-- several of a student's coaches. Today coaching_pbvision_jobs.vid is UNIQUE, so
-- one video = one job = one coach. Swap that for a (vid, coach_student_id) unique
-- index so each coach thread gets its OWN job row referencing the SAME PB Vision
-- video — analyzed once (billed once), viewable by every coach it's shared to.
--
-- The Go code uses a manual upsert (select-then-insert/update) rather than
-- on_conflict, so it works on both sides of this migration: before it runs,
-- single-coach analysis keeps working and extra-coach shares simply no-op (the
-- old vid-unique blocks the second row); after it runs, shares land.
alter table coaching_pbvision_jobs
  drop constraint if exists coaching_pbvision_jobs_vid_key;
create unique index if not exists coaching_pbvision_jobs_vid_thread_uidx
  on coaching_pbvision_jobs (vid, coach_student_id);
