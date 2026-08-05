-- Per-coach preferences: which Goals-tab sections a coach uses.
-- Both the coach's and the student's Goals tab honor these; a missing row means
-- everything is shown (default true), so this is safe to ship before backfill.
create table if not exists coach_settings (
  coach_id           uuid primary key,
  show_progress      boolean     not null default true,
  show_achievements  boolean     not null default true,
  show_skill_ratings boolean     not null default true,
  updated_at         timestamptz not null default now()
);
