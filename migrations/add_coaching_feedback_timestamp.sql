-- Timestamped feedback: pin a comment to a moment in the clip (e.g. 4:20) so the
-- other party can tap it and jump straight there. Nullable = untimed comment.
alter table coaching_feedback add column if not exists timestamp_seconds numeric;
