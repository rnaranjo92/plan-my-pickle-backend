-- Stores the DUPR matchCode returned when a sanctioned match is submitted, so we
-- (a) never double-submit (idempotency) and (b) can update/delete it on DUPR
-- later if a score is corrected.
alter table matches add column if not exists dupr_match_code text;
