-- The rest of a player's card styling: which display face, and which surface
-- pattern. Colour alone made every card the same object in a different paint.
--
-- Keys, not values — 'condensed' / 'court' rather than a font family or a set of
-- coordinates — so the client owns how each looks and they can all be restyled
-- without rewriting anyone's choice.
alter table pmp_profiles
  add column if not exists card_font text not null default '';

alter table pmp_profiles
  add column if not exists card_pattern text not null default '';

comment on column pmp_profiles.card_font is
  'Player card typeface key. Empty = the condensed house default.';
comment on column pmp_profiles.card_pattern is
  'Player card surface pattern key. Empty = plain gradient.';
