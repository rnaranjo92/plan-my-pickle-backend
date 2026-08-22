-- Public vs private clubs (Kim, 2026-08-22).
--
-- Until now EVERY club was effectively public: anyone signed in could ask to
-- join any club they could reach, and any club that had run an event appeared
-- in the city directory and the sitemap. A club that is a private group of
-- friends, or a facility that only wants members it has invited, had no way to
-- say so.
--
-- DEFAULT TRUE, deliberately. Every club that exists today was created under
-- the old rule and is already listed and joinable; defaulting to false would
-- silently pull them out of the directory and switch off their join button,
-- which is a change nobody asked for made on their behalf.
--
-- What private means:
--   • no join requests — the Ask to join button is gone, and the API refuses
--   • out of the public directory, the city hubs and the sitemap
--   • STILL reachable by direct link, because that is how an invitation works:
--     an invited person taps a link, and a page they cannot open is not an
--     invitation. They see the club and are admitted by the invite itself.
--
-- HOW TO RUN: Supabase dashboard → SQL Editor → paste → Run. Idempotent.

alter table clubs
  add column if not exists is_public boolean not null default true;

-- Verify — expect every existing club to read true.
select id, name, is_public from clubs order by created_at desc limit 20;
