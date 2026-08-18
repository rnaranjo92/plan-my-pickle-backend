-- Club join requests: asking to join, rather than simply appearing.
--
-- Until now anyone with the link could add themselves to a club's roster. For a
-- community club that's the point; for a facility charging dues it means the
-- member list — and the "24 of 31 paid" count built on it — is self-serve.
--
-- A SEPARATE TABLE rather than a status column on club_members, deliberately.
-- club_members is read in a dozen places (member counts, the roster, announce
-- recipients, dues, the org rollup, co-owner checks). A 'pending' row living in
-- that table would be correct only in the places that remembered to filter it
-- out, and every place that forgot would silently count a stranger as a member.
-- Keeping requests outside means every existing query stays right by
-- construction, with no filter to forget.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run. Idempotent.
-- Everything is inert until this runs: joins keep working exactly as they do
-- today (immediate), so this can be deployed before the migration.

create table if not exists club_join_requests (
  club_id      uuid not null references clubs(id) on delete cascade,
  user_id      text not null,
  -- invited = the club asked for THEM. They skip the queue on accepting:
  -- making an admin approve someone they personally invited is asking the same
  -- question twice.
  invited      boolean not null default false,
  requested_at timestamptz not null default now(),
  primary key (club_id, user_id)
);

create index if not exists club_join_requests_club_idx
  on club_join_requests (club_id, requested_at desc);

alter table club_join_requests enable row level security;

-- Verify:
--   select count(*) from club_join_requests;
