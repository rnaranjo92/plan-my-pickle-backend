-- Platform denylist: phone numbers / emails blocked from registering anywhere on
-- PlanMyPickle. Checked at registration (self AND organizer-add) before a player
-- row or pending request is created. Managed by the platform owner from the app.
-- phone is stored NORMALIZED (digits only, US leading-1 dropped) to match the
-- same normalization applied to an incoming registration; email is lowercased.
create table if not exists blocked_contacts (
  id         uuid        primary key default gen_random_uuid(),
  phone      text,                      -- normalized digits (nullable)
  email      text,                      -- lowercased (nullable)
  reason     text,
  created_at timestamptz not null default now()
);
create index if not exists blocked_contacts_phone_idx on blocked_contacts (phone);
create index if not exists blocked_contacts_email_idx on blocked_contacts (email);
