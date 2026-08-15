-- City on events, for city-level tournament pages.
--
-- Events already carry county+state, which powers /pickleball-tournaments/
-- {state}/{county}. But nobody searches "pickleball tournaments san diego
-- county" — they search "pickleball tournaments san diego". The county page
-- targets a phrase real people don't type.
--
-- City is stamped from the venue coordinates at create time and by the hourly
-- geo-repair pass, exactly like county. Nullable: an event with no coords, or
-- one created before this column existed, simply has no city and falls back to
-- its county page.
alter table events add column if not exists city text;

-- The city pages filter listed events by city+state, same shape as the county
-- hub's scan.
create index if not exists events_city_state_idx
  on events (state, city) where listed = true;

comment on column events.city is
  'Municipality from reverse-geocoding the venue. Powers city-level SEO pages; county stays the fallback.';
