-- Club waiver: a NOTICE, not a document.
--
-- Kim's decision (2026-08-17): PlanMyPickle does not want to be in the waiver
-- business. So this stores exactly two things — whether the club requires one,
-- and where to find it — and nothing else. No waiver text, no signatures, no
-- record of who accepted what, no dates. The club collects and keeps its own
-- waivers exactly the way it does today; the app only tells players a waiver
-- exists so nobody turns up to a session and finds out at the net.
--
-- That boundary is the point, and it is worth keeping. The moment the app
-- stores a signature it becomes the custodian of a legal document: it has to be
-- able to produce it years later, prove it wasn't altered, and stand behind it
-- if anyone disputes it. A link and a flag carry none of that.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run. Idempotent.

alter table clubs add column if not exists requires_waiver boolean not null default false;

-- Optional. A club can require a waiver it hands out on paper at the door, so
-- the URL is not implied by the flag and the flag is not implied by the URL.
alter table clubs add column if not exists waiver_url text;

-- Verify:
--   select name, requires_waiver, waiver_url from clubs limit 5;
