-- Two-tier no-show reminders for class enrollees (~24h and ~1h before start),
-- each sent at most once per enrollment.
alter table coaching_enrollments add column if not exists reminded_24h boolean not null default false;
alter table coaching_enrollments add column if not exists reminded_1h boolean not null default false;
