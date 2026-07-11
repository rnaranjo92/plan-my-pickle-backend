-- On-deck push dedup guard.
--
-- The on-deck ("you're next") push is deduped by inserting a notifications row
-- with type='on_deck' for the announced match. StartMatch fires from more than
-- one path (manual organizer start + auto-advance), so two near-simultaneous
-- starts on a court can race the SELECT-then-INSERT dedup and double-notify.
--
-- This partial unique index makes the INSERT itself the atomic claim: a second
-- concurrent insert for the same match 409s, and the losing goroutine skips the
-- push. Partial (WHERE type='on_deck') so it does NOT constrain the per-phone
-- game_starting / score_confirm rows, which legitimately repeat per recipient.
CREATE UNIQUE INDEX IF NOT EXISTS notifications_on_deck_once
  ON notifications (match_id)
  WHERE type = 'on_deck';
