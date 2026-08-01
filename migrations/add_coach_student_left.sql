-- A student can end (soft-archive) a coaching relationship: left_at is stamped
-- so the thread hides from both rosters (coach + student) while the clip history
-- is preserved. Coaches still hard-remove via DELETE /coach/students/{id}.
alter table coach_students add column if not exists left_at timestamptz;
