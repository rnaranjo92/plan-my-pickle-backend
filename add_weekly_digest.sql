-- Weekly "what's on near you" digest.
--
-- digest_opt_out is the unsubscribe. It is checked before every send and set by
-- the one-click link in the email footer -- a bulk email without a working
-- unsubscribe is a CAN-SPAM violation, not a missing nicety.
--
-- digest_sent_on is a per-person claim, so a job that dies halfway can be re-run
-- without emailing the people it already reached.
--
-- Safe to re-run.
alter table pmp_profiles
  add column if not exists digest_opt_out boolean not null default false,
  add column if not exists digest_sent_on date;
