-- claim_token — lets a player CLAIM a roster row that was created as a bare name.
--
-- Why: an organizer can now drop names straight onto a roster ("just put names
-- in and see the games"). Those rows have no phone, no email and no account, so
-- when that person later joins the app nothing links them — their matches,
-- standings and history sit on an orphaned row forever. This token is how the
-- row finds its owner.
--
-- Deliberately NOT check_in_token: that one is handed out for QR check-in, and
-- anyone holding it could then check players in. Claiming an identity and
-- checking in are different privileges, so they get different secrets.
--
-- Nullable and minted on demand (only when an organizer asks for a link), so
-- existing rows are untouched and we don't generate secrets nobody uses.
alter table registrations add column if not exists claim_token text;

-- Unique so a token identifies exactly one registration; partial so the many
-- rows with no token don't collide with each other.
create unique index if not exists idx_registrations_claim_token
  on registrations (claim_token)
  where claim_token is not null;
