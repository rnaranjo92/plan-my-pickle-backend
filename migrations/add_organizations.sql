-- Organizations: one layer above clubs, for operators who run several.
--
-- WHY: a club is a room full of people who know each other. A chain is not —
-- Life Time runs ~180 locations, and "sign up 180 times with 180 cards" is not
-- how that buys anything. What corporate needs is one contract, one place to
-- see every site, and staff whose access follows their job rather than whoever
-- happened to create the club.
--
-- Deliberately NOT a rename of clubs. A club keeps working exactly as it does
-- today, owned by a person, with or without an organization above it — most
-- clubs will never have one, and the single-club organizer must not pay for a
-- concept they don't need.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run. Idempotent.

create table if not exists organizations (
  id          uuid primary key default gen_random_uuid(),
  name        text not null,
  owner_id    text not null,
  -- The billing contact and the support contact are usually different people
  -- at this size, and neither is necessarily the owner account.
  contact_email text,
  created_at  timestamptz not null default now()
);

-- Staff, and what they may do. Roles mirror clubs so there is one mental model:
--   'owner' — the account on the contract. Everything, including deleting.
--   'admin' — runs sites day to day: manage any club in the org, create events.
--   'viewer' — reporting only. Exists because a regional manager who needs the
--              numbers should not be handed the ability to delete a season.
create table if not exists organization_members (
  org_id     uuid not null references organizations(id) on delete cascade,
  user_id    text not null,
  role       text not null default 'viewer',
  created_at timestamptz not null default now(),
  primary key (org_id, user_id)
);

create index if not exists organization_members_user_idx
  on organization_members (user_id);

-- A club may belong to one organization. NULL is the normal case and stays the
-- normal case: every club that exists today keeps working untouched.
alter table clubs add column if not exists org_id uuid
  references organizations(id) on delete set null;

create index if not exists clubs_org_idx on clubs (org_id);

-- ON DELETE SET NULL, not CASCADE, and this is the important line in the file:
-- deleting an organization must never delete the clubs inside it. A contract
-- ending is a billing event; the clubs, their members and their history belong
-- to the people who play there. The worst case of getting this wrong is
-- unrecoverable, so it is spelled out rather than defaulted.

alter table organizations enable row level security;
alter table organization_members enable row level security;

-- Verify:
--   select count(*) from organizations;
--   select column_name from information_schema.columns
--   where table_name = 'clubs' and column_name = 'org_id';
