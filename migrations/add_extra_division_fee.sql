-- Multi-division pricing: how the entry fee applies when a player enters more
-- than one division of the same event. The player's FIRST (earliest) registration
-- always pays the full registration_fee_cents; ADDITIONAL divisions follow the
-- mode below. Organizer picks this at tournament creation; discount is default.
--   discount = additional divisions cost additional_division_fee_cents
--   free     = additional divisions are $0
--   full     = additional divisions pay the full entry fee again
alter table events
  add column if not exists extra_division_fee_mode text not null default 'discount'
    check (extra_division_fee_mode in ('discount', 'free', 'full')),
  add column if not exists additional_division_fee_cents integer not null default 0
    check (additional_division_fee_cents >= 0);
