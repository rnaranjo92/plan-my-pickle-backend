-- The colourway a player picked for their own card.
--
-- On the PROFILE, not in local storage: a card that reverts to default when you
-- open the app on a different device isn't customised, it's decorated. Stored as
-- a short key ('navy', 'sunset', …) rather than colours, so the palettes can be
-- restyled later without rewriting anyone's choice.
alter table pmp_profiles
  add column if not exists card_theme text not null default '';

comment on column pmp_profiles.card_theme is
  'Player card colourway key. Empty = the default navy/green.';
