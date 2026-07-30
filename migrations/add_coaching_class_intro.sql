-- Mark a class as an intro / trial for first-timers (often free or discounted).
-- Shown with an "Intro" badge so newcomers know where to start.
alter table coaching_classes add column if not exists is_intro boolean not null default false;
