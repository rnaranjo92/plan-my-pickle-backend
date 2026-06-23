-- DUPR account connections (one per signed-in user), populated by the SSO
-- consent flow. DUPR is a "restricted partner": data is only accessible for
-- users who connect their DUPR account via SSO. The user/refresh tokens let us
-- call DUPR on the user's behalf; ratings are cached here and refreshed via
-- webhooks. Backend (service role) is the only writer — anon/authenticated have
-- no grants, so the tokens never leave the server.
create table if not exists dupr_connections (
  user_id        uuid primary key references auth.users(id) on delete cascade,
  dupr_id        text not null,
  user_token     text,
  refresh_token  text,
  token_expiry   timestamptz,
  doubles_rating numeric,
  singles_rating numeric,
  connected_at   timestamptz not null default now(),
  updated_at     timestamptz not null default now()
);

create index if not exists dupr_connections_dupr_id_idx on dupr_connections (dupr_id);

alter table dupr_connections enable row level security;
-- No anon/authenticated policies: only the service-role backend reads/writes.
