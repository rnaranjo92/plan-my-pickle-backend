-- Decouple SMS consent from phone presence.
-- Phone is now REQUIRED at registration (organizers need a way to reach players),
-- so "has a stored phone" can no longer imply "opted in to automated texts".
-- This column is the explicit consent that gates all platform SMS (court calls,
-- schedule alerts, score confirmations).
--
-- Backfill: every existing player who already has a stored phone consented under
-- the prior "phone present = consent" model, so keep texting them.
alter table players add column if not exists sms_consent boolean not null default false;

update players
set sms_consent = true
where sms_consent = false
  and phone is not null
  and btrim(phone) <> '';
