-- Organizer email branding (Premium): logo, accent color, and signature applied
-- to every outgoing email for an event when the owner is Premium. All optional;
-- NULL = the PlanMyPickle default look. Referenced only-when-set by the backend,
-- so deploying the code before this migration is safe.
alter table events add column if not exists email_brand_logo_url text;
alter table events add column if not exists email_brand_color    text;
alter table events add column if not exists email_signature      text;
