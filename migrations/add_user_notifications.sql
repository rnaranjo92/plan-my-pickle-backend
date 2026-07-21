-- Facebook-style per-user notification feed. Distinct from the `notifications`
-- table (that's a per-message SMS/push DELIVERY log); this is the in-app
-- "activity" list a user sees under the bell: someone followed you, reacted to
-- or commented on your post, or registered for your event.
--
-- recipient_id = who sees it. actor_id/actor_name = who caused it (nullable for
-- system notices). type drives the icon + copy. link = an app deep-link target
-- (e.g. event:<id> / profile:<id> / feed) the client routes on tap. read flips
-- when the user opens the center.
create table if not exists user_notifications (
  id            uuid primary key default gen_random_uuid(),
  recipient_id  uuid not null,
  type          text not null,
  actor_id      uuid,
  actor_name    text not null default '',
  title         text not null default '',
  body          text not null default '',
  link          text not null default '',
  read          boolean not null default false,
  created_at    timestamptz not null default now()
);

-- The bell's list + unread badge both query by recipient, newest first.
create index if not exists user_notifications_recipient_idx
  on user_notifications (recipient_id, created_at desc);

-- Unread-count fast path.
create index if not exists user_notifications_unread_idx
  on user_notifications (recipient_id) where read = false;
