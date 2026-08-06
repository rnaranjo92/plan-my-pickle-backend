-- League membership: players join a league ONCE and are auto-rostered into every
-- session (recurring Round Robin nights). Mirrors coach_students: added by name +
-- email/phone, invited via text/email, claimed by token → user_id set. Subs are
-- handled per-session by editing that night's event roster (a sub is just a
-- one-week guest), so membership itself stays simple. Safe pre-run (feature dark).
create table if not exists league_members (
  id           uuid primary key default gen_random_uuid(),
  league_id    uuid not null references leagues(id) on delete cascade,
  user_id      uuid,            -- linked account once resolved/claimed
  full_name    text,
  email        text,
  phone        text,
  invite_token text unique,
  left_at      timestamptz,     -- soft-remove (hidden from the roster)
  created_at   timestamptz not null default now()
);
create index if not exists league_members_league_idx on league_members(league_id);

-- Default court count a league's sessions use for scheduling (nil = per-session).
alter table leagues add column if not exists court_count integer;
