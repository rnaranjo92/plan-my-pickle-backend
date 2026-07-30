-- Saved/favorite coaches: a player bookmarks coaches to find them again fast.
create table if not exists coach_favorites (
  id            uuid primary key default gen_random_uuid(),
  user_id       text not null,
  coach_user_id text not null,
  created_at    timestamptz not null default now(),
  unique (user_id, coach_user_id)
);
create index if not exists coach_favorites_user_idx on coach_favorites (user_id);
alter table coach_favorites enable row level security;
