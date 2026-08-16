-- Device-level push addresses, so a send never depends on OneSignal's identity
-- model resolving.
--
-- Every targeted notification used to go out via include_aliases.external_id.
-- That works right up until the alias is wedged: a login-user 409 against a user
-- that already holds the external_id pauses the OneSignal SDK's op queue,
-- leaving a live subscription owned by no addressable user. The send is then
-- ACCEPTED and delivered to nobody — the alias did resolve, just to a user with
-- no devices. Silent, and indistinguishable from "nothing to send".
--
-- A subscription id addresses one device directly. The client writes its own
-- here once it has verified the subscription is opted in, and sends prefer these
-- over the alias.
create table if not exists push_subscriptions (
  -- OneSignal's subscription id. PRIMARY KEY: the same device re-registering
  -- must update its row, not accumulate duplicates that each get a copy of
  -- every court call.
  subscription_id text primary key,
  user_id text not null,
  platform text not null default '',
  updated_at timestamptz not null default now()
);

-- Sends look up by user; this is the only access pattern.
create index if not exists push_subscriptions_user_idx
  on push_subscriptions (user_id);

comment on table push_subscriptions is
  'Per-device OneSignal subscription ids. Preferred over external_id aliases '
  'for targeted sends; rows are pruned when OneSignal reports an id invalid.';
