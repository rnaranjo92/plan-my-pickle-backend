-- Per-division court assignment.
-- An empty/NULL `courts` means the division may use any court (current behavior).
-- A non-empty set (e.g. {1,2}) pins that division's games to only those courts
-- when the scheduler arranges them (STRICT: it never spills onto other courts).
ALTER TABLE brackets ADD COLUMN IF NOT EXISTS courts int[];
