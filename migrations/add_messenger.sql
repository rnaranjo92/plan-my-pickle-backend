-- Facebook Messenger channel — a free court-call path for players who opt in by
-- opening a conversation with our Page (the check-in QR is an m.me?ref= link).
-- messenger_psid is Meta's Page-Scoped ID for that player; messenger_last_in is
-- their last inbound time, which opens the 24-hour standard messaging window we
-- send court calls inside. Inert until MESSENGER_PAGE_TOKEN is configured.
alter table players add column if not exists messenger_psid text;
alter table players add column if not exists messenger_last_in timestamptz;

-- We look players up by PSID on every inbound webhook (to refresh the window),
-- so index it. Partial: only the rows that actually opted in.
create index if not exists players_messenger_psid_idx
  on players (messenger_psid) where messenger_psid is not null;
