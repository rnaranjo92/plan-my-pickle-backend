-- Instructor Mode: per-skill RATINGS the coach assesses for a student. DUPR is a
-- single composite with no sub-scores, so this is the rubric layer. Six canonical
-- skills (matching the USAP matrix + PB Vision axes): serve, return, dinks, drops
-- (third-shot), volleys (volleys/resets), strategy (strategy/IQ). Each rated 1-5.
-- first_rating is captured the first time a skill is set, so the player's Progress
-- view can show "since you started". One row per (student, skill). Guarded by
-- columnReady, so deploying before this runs is safe (the skills card stays empty).
create table if not exists coaching_skill_ratings (
  id               uuid primary key default gen_random_uuid(),
  coach_student_id uuid not null references coach_students (id) on delete cascade,
  skill            text not null,          -- serve|return|dinks|drops|volleys|strategy
  rating           numeric(2,1) not null,  -- 1.0 - 5.0
  first_rating     numeric(2,1),           -- captured on first set (for "since start")
  updated_at       timestamptz not null default now(),
  unique (coach_student_id, skill)
);
create index if not exists coaching_skill_ratings_cs_idx
  on coaching_skill_ratings (coach_student_id);
alter table coaching_skill_ratings enable row level security;
-- Service-role only (the Go backend); no anon/authenticated policy.
