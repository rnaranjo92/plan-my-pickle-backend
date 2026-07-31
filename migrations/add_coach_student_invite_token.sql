-- Invite token that binds a coach<->student relationship on ANY account claim,
-- so an invited student who signs up with a different email/phone than the coach
-- typed still links (the previous exact-match-only auto-link failed silently).
alter table coach_students
  add column if not exists invite_token text;
create unique index if not exists coach_students_invite_token_idx
  on coach_students (invite_token) where invite_token is not null;
