-- Anti-dummy-registration controls: invite code + approval queue.
-- Both are opt-in per event and fully backward-compatible — the backend reads
-- each column only after a columnReady probe confirms it exists, so deploying
-- the code before this migration is safe (the features simply stay off).
--
-- events.registration_code: when non-empty, a self-registering player must
--   supply the matching code (organizers share it with real entrants only).
--   Blank (default) = open registration, unchanged behavior.
-- events.require_approval: when true, a self-registration lands NOT approved and
--   is held out of the roster/draw/counts until an organizer approves it.
-- registrations.approved: false only while a registration awaits approval. Every
--   pre-existing row and every organizer-added registration is approved
--   (default true), so nothing already in the system is affected.
alter table events add column if not exists registration_code text not null default '';
alter table events add column if not exists require_approval boolean not null default false;
alter table registrations add column if not exists approved boolean not null default true;
