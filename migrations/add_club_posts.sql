-- Posts that belong to a CLUB rather than to one of its events (Kim,
-- 2026-08-22: "make the club feed like the main feed — post videos, photos,
-- text").
--
-- Until now the club feed was purely DERIVED: it read the feed items of the
-- club's events, so the club itself had no voice — nothing could be said to
-- the club that wasn't about a particular session. This adds the column that
-- lets a feed_items row belong to the club directly.
--
-- ON DELETE CASCADE: a deleted club takes its posts with it. Event-derived
-- items are untouched (their club_id is null; they belong to their event).
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent.
-- Until it runs, posting returns a clear error and the feed keeps working
-- exactly as before.

alter table feed_items
  add column if not exists club_id uuid references clubs(id) on delete cascade;

create index if not exists feed_items_club_idx on feed_items (club_id);

-- Verify — expect the column to exist:
select column_name from information_schema.columns
where table_name = 'feed_items' and column_name = 'club_id';
