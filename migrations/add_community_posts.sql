-- Community (user) posts + a county-scoped "Near you" NewsFeed.
-- Run in the Supabase SQL Editor.

-- A feed_item can now be a standalone USER post (no event): allow a null
-- event_id, and add the author + a denormalized county/state for the nearby
-- query. (actor_name already exists and is reused for the author's display name.)
alter table feed_items alter column event_id drop not null;
alter table feed_items add column if not exists author_id uuid;
alter table feed_items add column if not exists county text;
alter table feed_items add column if not exists state text;

-- Where an event/venue is, so its activity can surface in a county feed.
alter table events add column if not exists county text;
alter table events add column if not exists state text;

-- The user's chosen home location (they enter a zip; we geocode it to
-- county/state) — powers the county-scoped feed.
alter table pmp_profiles add column if not exists home_zip text;
alter table pmp_profiles add column if not exists county text;
alter table pmp_profiles add column if not exists state text;

create index if not exists feed_items_nearby_idx
  on feed_items (state, county, created_at desc);
