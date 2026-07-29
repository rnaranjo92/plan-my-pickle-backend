-- Instructor Mode: a coach's private "running note" ABOUT a student (goals,
-- level, focus areas) — distinct from the per-clip notes. Coach-only: never
-- included in the student's own view of the relationship. Guarded by columnReady,
-- so safe to deploy before this runs (the field just stays hidden).
alter table coach_students
  add column if not exists coach_note text;
