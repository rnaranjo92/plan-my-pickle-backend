-- A reviewed coach can post one public reply per review of them, shown under the
-- review on their marketplace profile + in their reviews inbox.
alter table coach_reviews add column if not exists coach_response text;
