-- On-deck SMS is an independent opt-in ON TOP of the sms_notifications "both
-- channels" add-on. Default false: even an SMS-enabled event does NOT text the
-- on-deck warm-up heads-up (which ~doubles court-call SMS volume) unless the
-- organizer turns this on. When off, on-deck stays push-only (free) as before.
-- Read/write is columnReady-guarded in the service, so pre-migration it's inert.
alter table events add column if not exists ondeck_sms boolean not null default false;
