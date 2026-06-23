-- ============================================================================
-- make_all_events_discoverable.sql
-- Surfaces EVERY game (event) in PlanMyPickle's public discovery.
--
-- "Discoverable" = events.listed = true. Both public feeds filter on it:
--   * PublicEvents (planmypickle.com marketing feed):  listed = true
--   * NearbyEvents (in-app Nearby map/list):            listed = true
--                                                       AND venue_lat/lng NOT NULL
-- So this flips listed = true everywhere. Events that also have coordinates show
-- on the map immediately; ones with only a text location appear in the list and
-- need geocoding for a map pin (see note at the bottom).
--
-- Safe to re-run (idempotent). Run in the Supabase SQL Editor.
-- ============================================================================

-- 1) BEFORE -- current state.
SELECT
  count(*)                                             AS total_events,
  count(*) FILTER (WHERE listed)                       AS already_discoverable,
  count(*) FILTER (WHERE listed IS DISTINCT FROM true) AS not_yet_discoverable
FROM events;

-- 2) Make every event discoverable. (IS DISTINCT FROM true covers false + NULL.)
UPDATE events
SET    listed = true
WHERE  listed IS DISTINCT FROM true;

-- 3) AFTER -- confirm, and flag events that won't get a MAP PIN yet (no coords).
SELECT
  count(*)                                                                    AS total_events,
  count(*) FILTER (WHERE listed)                                              AS discoverable,
  count(*) FILTER (WHERE listed AND (venue_lat IS NULL OR venue_lng IS NULL)) AS listed_but_no_map_pin
FROM events;

-- ---------------------------------------------------------------------------
-- OPTIONAL: only want UPCOMING games discoverable (not past ones)? Replace
-- step 2 with:
--
--   UPDATE events SET listed = true
--   WHERE  listed IS DISTINCT FROM true
--     AND  (starts_at IS NULL OR starts_at >= now());
-- ---------------------------------------------------------------------------
