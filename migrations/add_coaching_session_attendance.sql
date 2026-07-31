-- Attendance/no-show tracking for booked 1:1 sessions (coach marks it after).
-- null = not marked, 'attended', 'no_show'.
alter table coaching_schedule
  add column if not exists status text;
