-- One-time "welcome to SMS" dedupe flag.
-- The first time a player opts into texts (sms_consent) with a phone on file,
-- the backend sends a single confirmation SMS (STOP/HELP included). This flag
-- records that it was sent so re-registering for another event never re-texts.
alter table players add column if not exists sms_welcomed boolean not null default false;

-- Backfill: anyone already consented has been receiving platform texts under the
-- prior model, so treat them as already-welcomed — the welcome must only fire for
-- genuinely NEW opt-ins going forward. Players not yet consented stay false, so if
-- they opt in later they still get their one welcome.
update players
set sms_welcomed = true
where sms_consent = true;
