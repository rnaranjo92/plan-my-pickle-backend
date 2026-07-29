-- Instructor Mode: let a coach invite a student by PHONE (text) as well as email.
-- A roster row now needs at least one of email/phone; the student links to their
-- account when they sign up with that email OR that phone. student_phone is stored
-- as the last-10 digits for reliable matching against the account's phone.
alter table coach_students alter column student_email drop not null;
alter table coach_students add column if not exists student_phone text;
create index if not exists coach_students_phone_idx on coach_students (student_phone);
