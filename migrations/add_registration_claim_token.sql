-- Claiming a roster row that was created as a bare name.
--
-- Why: an organizer can drop names straight onto a roster ("just put names in
-- and see the games"). Those rows have no phone, no email and no account, so
-- when that person later joins the app nothing links them — their matches,
-- standings and history sit on an orphaned row forever. These tokens are how a
-- row finds its owner.

-- 1) PER-PLAYER claim link. The organizer shares it with one person; opening it
--    signed in binds that exact row. Minted on demand and burned on use.
--
--    Deliberately NOT check_in_token: that one is handed out for QR check-in,
--    and anyone holding it could then check players in. Claiming an identity and
--    checking someone in are different privileges, so different secrets.
alter table registrations add column if not exists claim_token text;

create unique index if not exists idx_registrations_claim_token
  on registrations (claim_token)
  where claim_token is not null;

-- 2) PER-EVENT claim code, for the court-side "pick your name" QR.
--
--    This is the secret the QR actually carries. Without it, knowing an event id
--    would be enough to list a stranger's roster and take an unclaimed name —
--    and event ids travel in ordinary share links, so they are not secret at
--    all. The self-serve flow is only acceptable because holding the code means
--    you were standing where the organizer posted it.
--
--    Non-league events only (enforced in code): on a league, standings persist
--    for a season and every player already has a contact to match on.
alter table events add column if not exists claim_code text;

create unique index if not exists idx_events_claim_code
  on events (claim_code)
  where claim_code is not null;
