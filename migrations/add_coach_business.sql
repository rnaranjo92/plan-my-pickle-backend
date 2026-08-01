-- Coach's business name + street address (for their "Find a coach" listing and
-- as a more precise geocode source than city for the map pin).
alter table coach_profiles add column if not exists business_name text;
alter table coach_profiles add column if not exists address text;
