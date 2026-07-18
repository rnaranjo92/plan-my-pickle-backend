-- Optional cap on how many distinct players may register for an event.
-- NULL = unlimited. Enforced on self-registration (organizers may exceed it).
alter table events add column if not exists max_players int;
