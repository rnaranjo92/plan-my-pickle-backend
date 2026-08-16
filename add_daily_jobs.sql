-- Marker for jobs that must run at most once per calendar day.
--
-- The joke-of-the-day push is scheduled as ONE OneSignal notification with
-- per-timezone delivery, so sending it twice means every user gets two. An
-- in-memory guard isn't enough: a redeploy resets it, and Railway redeploys
-- whenever main moves — which today was a dozen times.
create table if not exists daily_jobs (
  name text primary key,
  ran_on date not null
);

comment on table daily_jobs is
  'At-most-once-per-day marker. Claim by UPDATE ... WHERE ran_on < today.';
