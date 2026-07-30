-- Coach reviews: a player's 1-5 star rating + comment for a coach. One review
-- per (coach, author). Eligibility (the author actually trained with the coach)
-- is enforced in the Go layer, not the DB. Powers marketplace social proof.
create table if not exists coach_reviews (
  id            uuid primary key default gen_random_uuid(),
  coach_user_id text not null,
  author_id     text not null,
  author_name   text,
  rating        int  not null check (rating between 1 and 5),
  body          text,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  unique (coach_user_id, author_id)
);
create index if not exists coach_reviews_coach_idx on coach_reviews (coach_user_id);

alter table coach_reviews enable row level security;
-- Service-role only; the Go layer authorizes reads (public) and writes (eligible
-- students only). No anon/authenticated policy on purpose.
