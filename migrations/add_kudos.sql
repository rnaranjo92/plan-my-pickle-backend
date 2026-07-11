-- Kudos: always-positive peer recognition. One account (giver_user_id) gives a
-- labeled kudos to another account (receiver_user_id). Account-to-account so a
-- person's recognition aggregates across every event they play. The unique
-- (giver, receiver, label) index is the anti-spam gate: you can recognize a
-- given skill in a given player at most once (Street Cred later weights by the
-- distinct giver count).
CREATE TABLE IF NOT EXISTS kudos (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  giver_user_id    uuid NOT NULL,
  receiver_user_id uuid NOT NULL,
  label            text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT kudos_no_self CHECK (giver_user_id <> receiver_user_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS kudos_unique
  ON kudos (giver_user_id, receiver_user_id, label);

CREATE INDEX IF NOT EXISTS kudos_receiver
  ON kudos (receiver_user_id);

-- Same lock-down as every other app-data table: RLS on, NO anon/authenticated
-- policy, so only the service-role backend (which bypasses RLS) can read/write.
-- Without this a public table is fully exposed on /rest/v1/kudos to the shipped
-- anon key — an attacker could forge givers, use off-list labels, and read the
-- whole table, bypassing all of GiveKudos's validation. Idempotent to re-run.
ALTER TABLE kudos ENABLE ROW LEVEL SECURITY;
