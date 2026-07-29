-- Instructor Mode: capture a student's skill level (coach's assessment) on the
-- roster row — free-form so it fits a DUPR-style number ("3.5") or a word
-- ("Intermediate"). Coach-only, like the private notes. Guarded by columnReady,
-- so deploying before this runs is safe (the field just stays hidden).
alter table coach_students add column if not exists skill_level text;
