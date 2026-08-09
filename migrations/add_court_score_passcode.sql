-- Require the scorekeeper passcode on court-QR scoring. When true (default), a
-- player scanning a court QR must enter the event's scorekeeper (admin) passcode
-- before recording that court's score — closing the "anyone who sees the QR can
-- score" gap. Enforced only when an admin_passcode is actually set (otherwise
-- there's nothing to check, so scoring stays token-only and never bricks). The
-- organizer can toggle this off, or change the passcode, anytime in Edit.
alter table events
  add column if not exists court_score_passcode boolean not null default true;
