-- A club's street address, so a member can actually navigate to it.
--
-- The existing `city` column STAYS and stays a city: it slugs the public
-- directory pages (/pickleball-clubs/{city}) and heads the organization
-- report's City column. The city is derived from this address on save rather
-- than asked for twice.
--
-- Safe to re-run.
alter table clubs
  add column if not exists address text;
