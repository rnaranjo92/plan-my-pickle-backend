-- Scoreboard theming: per-event look for the live TV / read-only board.
-- Stores { bg, text, accent, font } (hex strings + a font-family key). Nullable;
-- absent = the default house theme. Run in the Supabase SQL Editor.
alter table events add column if not exists scoreboard_theme jsonb;
