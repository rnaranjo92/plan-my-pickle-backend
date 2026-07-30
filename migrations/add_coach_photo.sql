-- Instructor discovery photo: a dedicated portrait/headshot shown on the coach
-- card + tap-a-pin sheet, separate from the round account avatar so a coach can
-- present a purpose-shot photo. Falls back to the account avatar when unset.
alter table coach_profiles add column if not exists photo_url text;
