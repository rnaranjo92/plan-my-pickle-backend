-- Coaching Marketplace: a coach's years of experience on their discovery
-- profile. Listing now requires a bio + years of experience (enforced in the Go
-- service). Guarded by columnReady, so deploying before this runs is safe.
alter table coach_profiles add column if not exists years_experience integer;
