-- Duplicate event cards in the NewsFeed (Kim, 2026-08-22).
--
-- ensureEventPosts is a read-then-insert with nothing unique underneath it, so
-- two feed loads racing each other (the app opening and refreshing) could both
-- find no card for an event and both write one. It runs only for events you
-- OWN, which is why the organizer saw two identical cards and everyone else
-- saw one.
--
-- OPTIONAL. The app already collapses these at read time — one event renders
-- one card, oldest wins — so nothing is broken without this. What this adds is
-- stopping the table from accumulating more, and tidying what is already there.
--
-- Run in the Supabase SQL editor.

-- 1. Delete duplicate event cards, keeping the OLDEST for each event.
--    Only rows nobody has reacted to or commented on: engagement points at a
--    specific feed_items id, and deleting a card someone liked would silently
--    take their like with it. Anything with engagement is left alone — the
--    read-time collapse keeps it out of sight either way, and the index in
--    step 2 will simply refuse to build until it is dealt with by hand.
delete from feed_items f
where f.type = 'event'
  and exists (
    select 1 from feed_items keep
    where keep.type = 'event'
      and keep.event_id = f.event_id
      and (keep.created_at, keep.id) < (f.created_at, f.id)
  )
  and not exists (select 1 from feed_reactions r where r.feed_item_id = f.id)
  and not exists (select 1 from feed_comments c where c.feed_item_id = f.id);

-- 2. Make the duplicate impossible rather than merely tidied.
--    A partial unique index is what turns ensureEventPosts' race into a losing
--    insert instead of a second card. If this errors with "could not create
--    unique index", a duplicate with engagement survived step 1 — find it with
--    the query at the bottom and decide which card to keep by hand.
create unique index if not exists feed_items_one_event_card
  on feed_items (event_id)
  where type = 'event';

-- Duplicates that still have engagement, if step 2 refused to build:
--
--   select event_id, count(*), array_agg(id order by created_at)
--   from feed_items where type = 'event'
--   group by event_id having count(*) > 1;
