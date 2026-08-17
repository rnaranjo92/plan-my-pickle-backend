-- daily_jobs: one row per once-a-day background job, holding the date it last
-- ran, so exactly one server instance performs each day's run.
--
-- WHY THIS FILE EXISTS NOW: claimDailyJob has always logged "run
-- add_daily_jobs.sql" when the table is missing — but the migration was never
-- written. So the table never existed, claimDailyJob returned false on every
-- attempt forever, and every daily job (the 9am joke-of-the-day push) has been
-- silently disabled since the day it shipped. Nothing was wrong with the push
-- itself; it was never reached.
--
-- HOW THE CLAIM WORKS (and what the schema has to support):
--
--   update daily_jobs set ran_on = <today> where name = <job> and ran_on < <today>
--
-- The claim IS that WHERE clause, not a read-then-write: two instances booting
-- together would both read "hasn't run today" and both send. Whoever's UPDATE
-- moves the row owns the run. If no row moves, the code INSERTs — which claims
-- the very first run, and on a race loses to a duplicate-key error, which is
-- the correct outcome. That race is only safe because `name` is UNIQUE, so the
-- primary key below is load-bearing, not decoration.
--
-- Dates are UTC (claimDailyJob formats time.Now().UTC()). The joke's own
-- Pacific-anchored day is a separate concern handled in jokeDay().
--
-- Idempotent: safe to run more than once, and safe on a live database — it
-- creates a new table and touches nothing existing.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run.

create table if not exists daily_jobs (
  name    text primary key,
  ran_on  date not null
);

-- The service role is what the backend connects as. No RLS policy is needed
-- because this table is never touched by an end user's token — but RLS is
-- enabled anyway so an anon key can't read or write it if one ever reaches it.
alter table daily_jobs enable row level security;

-- Verify (should return one row per job AFTER the next run; empty until then):
--   select name, ran_on from daily_jobs;
--
-- The joke push runs on the hourly loop and claims the day the first time it
-- fires, so a row named 'joke_of_the_day' appearing here is the confirmation
-- that this worked. Railway will also log:
--   joke push: sent ... (claimed <date>, joke day <date>)
