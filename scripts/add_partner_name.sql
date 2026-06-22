-- Partner pairing: free-text partner name (when the partner isn't a registered
-- player). Registered pairs use the existing registrations.partner_id (a FK to
-- players); this column holds the typed-in name for an un-registered partner so
-- the Players-tab card can show "Partner: <name>".
--
-- Idempotent: safe to run more than once. Run in the Supabase SQL editor BEFORE
-- (or right as) the backend deploy that reads this column rolls out. The roster
-- query tolerates the column being absent, so running it late only delays
-- free-text partner notes from showing.
ALTER TABLE registrations ADD COLUMN IF NOT EXISTS partner_name text;
