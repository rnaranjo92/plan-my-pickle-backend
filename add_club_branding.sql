-- Club branding: the accent colour a club's own pages are drawn in, so members
-- see the club rather than PlanMyPickle. Stored as '#RRGGBB', validated
-- server-side because it reaches the public club page's markup.
--
-- Safe to re-run.
alter table clubs
  add column if not exists brand_color text;
