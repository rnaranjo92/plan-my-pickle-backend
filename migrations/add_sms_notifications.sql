-- Premium "both channels" notification setting. Default OFF → push-first (free).
-- A premium organizer can turn it on to ALSO send SMS (court calls / delay
-- updates) on top of push. Gated at the API to the premium allowlist.
alter table events add column if not exists sms_notifications boolean not null default false;
