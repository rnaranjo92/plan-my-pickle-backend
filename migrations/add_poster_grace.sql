-- Poster retention: a rolling grace period so detaching never deletes instantly.
--
-- THE PROBLEM THIS FIXES: retention counted from CREATION and protection was
-- "is it an event's poster right now". So a poster that had been an event's
-- banner for 40 days was deleted within the hour of being swapped out — an
-- organizer changing a banner to compare two options lost the original, and
-- re-attaching it from a stale gallery view left the event showing a 404.
--
-- Now the sweep stamps protected_until = now() + 30 days every time it sees a
-- poster in use, and skips anything still protected. The 30-day clock therefore
-- starts when a poster STOPS being used, not when it was made.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent.
-- Safe on a live DB and safe to run before the code ships (the column is
-- columnReady-guarded; until it exists the sweep behaves exactly as it does now).

alter table poster_generations
  add column if not exists protected_until timestamptz;

-- Existing in-use posters get their grace on the sweep's next pass, so nothing
-- needs backfilling.

-- Verify:
--   select count(*) from poster_generations where protected_until is not null;
