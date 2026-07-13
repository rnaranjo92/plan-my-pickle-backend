-- Per-division expected player count. Used to fill a division with that many
-- placeholder players so an organizer can generate & preview the schedule before
-- real registrations come in. NULL/0 = no target (the fill helper falls back to
-- its default of 16 per division).
alter table brackets add column if not exists player_count int;
