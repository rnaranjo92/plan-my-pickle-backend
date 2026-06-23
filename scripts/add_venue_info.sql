-- Host-entered "venue & info" fields shown to players/spectators: free-text
-- things-to-know (parking, restrooms, food, policies) + an optional waiver link.
alter table events add column if not exists venue_notes text;
alter table events add column if not exists waiver_url  text;
