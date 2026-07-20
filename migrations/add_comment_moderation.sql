-- Comment moderation (App Store Guideline 1.2): let any signed-in user REPORT an
-- objectionable feed comment and BLOCK an abusive author so they stop seeing that
-- person's comments. Organizers can already delete comments; reports flag them for
-- review. Both tables are additive; the backend degrades gracefully (report/block
-- become no-ops, comment filtering is skipped) until this migration is applied.

create table if not exists comment_reports (
  id          uuid primary key,
  comment_id  uuid not null references feed_comments(id) on delete cascade,
  reporter_id uuid not null,
  reason      text,
  created_at  timestamptz not null default now(),
  unique (comment_id, reporter_id)
);
create index if not exists comment_reports_comment_idx on comment_reports(comment_id);

create table if not exists user_blocks (
  id         uuid primary key,
  blocker_id uuid not null,
  blocked_id uuid not null,
  created_at timestamptz not null default now(),
  unique (blocker_id, blocked_id)
);
create index if not exists user_blocks_blocker_idx on user_blocks(blocker_id);
