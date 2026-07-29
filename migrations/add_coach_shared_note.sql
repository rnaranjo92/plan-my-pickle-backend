-- Instructor Mode: a SHARED note the coach writes for the student to SEE (unlike
-- coach_note, which is private/coach-only). Shows on both the coach's and the
-- student's thread. Guarded by columnReady, so deploying before this runs is safe
-- (the shared Notes card just stays empty).
alter table coach_students add column if not exists shared_note text;
