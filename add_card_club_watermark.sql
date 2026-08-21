-- A player may OPT IN to showing their club's logo as a faint watermark behind
-- their PlanMyPickle ID card. Stores the club id, or '' for none.
--
-- Opt-in is the point: a card is the player's own, and the server only accepts
-- a club they actually belong to.
--
-- Safe to re-run.
alter table pmp_profiles
  add column if not exists card_club_watermark text not null default '';
