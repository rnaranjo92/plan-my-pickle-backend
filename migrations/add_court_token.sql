-- Per-event "court token" — a scoped secret embedded in the court-scoring QR so
-- players can scan a court QR and record that court's live game WITHOUT the admin
-- passcode (the physical QR at the court is the access control, Scoreholio-style).
-- Generated lazily when the organizer opens the Court QR sheet; rotatable.
alter table events add column if not exists court_token text;
