-- AI poster gallery: a record of every poster a user generated, so they can
-- see them again in Tools.
--
-- Retention is 30 days: generated images are a scratchpad, not an archive.
-- The daily cleanup deletes the storage object + this row once it's older than
-- 30 days — EXCEPT when the url is still an event's active poster (deleting a
-- poster a real upcoming event is showing would 404 its page). So a poster you
-- actually used survives; the throwaway tries you didn't use get swept.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run. Idempotent.
-- Everything is inert until this runs: generation still works, it just doesn't
-- record a gallery row (columnReady-guarded).

create table if not exists poster_generations (
  id         uuid primary key default gen_random_uuid(),
  user_id    text not null,
  -- The event it was generated for (posters are always derived from an event).
  event_id   text,
  -- The public Storage URL of the generated image.
  url        text not null,
  -- The style key chosen, for a small label in the gallery ("Retro athletic").
  style      text,
  created_at timestamptz not null default now()
);

create index if not exists poster_generations_user_idx
  on poster_generations (user_id, created_at desc);

alter table poster_generations enable row level security;

-- Verify:
--   select count(*) from poster_generations;
