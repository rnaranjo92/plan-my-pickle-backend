-- Class packs: a coach sells N credits at a discount; a player buys a pack
-- (hosted checkout) and each credit covers one paid class enrollment.
create table if not exists coach_packs (
  id          uuid primary key default gen_random_uuid(),
  coach_id    text not null,
  title       text not null,
  credits     int  not null check (credits > 0),
  price_cents int  not null check (price_cents >= 0),
  active      boolean not null default true,
  created_at  timestamptz not null default now()
);
create index if not exists coach_packs_coach_idx on coach_packs (coach_id);

-- A player's remaining credit balance with a specific coach.
create table if not exists coaching_credits (
  id                uuid primary key default gen_random_uuid(),
  coach_id          text not null,
  user_id           text not null,
  credits_remaining int  not null default 0,
  updated_at        timestamptz not null default now(),
  unique (coach_id, user_id)
);

alter table coach_packs      enable row level security;
alter table coaching_credits enable row level security;
-- Service-role only; the Go layer authorizes.
