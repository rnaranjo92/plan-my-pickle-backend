-- Verified coaches: a trust badge we grant after vetting, plus a coach-entered
-- certifications line (e.g. "PPR Certified · IPTPA Level 2").
alter table coach_profiles add column if not exists verified boolean not null default false;
alter table coach_profiles add column if not exists certifications text;
