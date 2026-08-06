-- League-level recurring schedule: "this league runs every <weekday> at <time>,
-- forever." Under the hood it points at ONE recurring Round-Robin session event
-- (recur_event_id) whose weekly occurrences auto-roster the league's members.
-- recur_start_at is the anchor (weekday + time derived from it for display).
-- Safe pre-run (feature dark).
alter table leagues add column if not exists recurs boolean not null default false;
alter table leagues add column if not exists recur_event_id uuid;
alter table leagues add column if not exists recur_start_at timestamptz;
