-- Organizer's proposed per-round start times: {"<roundNumber>": <minuteOfDay>}.
-- The client schedule cascade anchors each round to its proposed time on Build;
-- no wall-clock is stored on matches. NULL/absent = auto-pack (today's behavior).
alter table events add column if not exists round_start_minutes jsonb;
