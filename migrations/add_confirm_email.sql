-- Organizer-customized registration-confirmation email. Subject overrides the
-- default "You're in! …"; message is a personal note added to the top of the
-- confirmation body. Both null/empty → the branded default copy.
alter table events add column if not exists confirm_email_subject text;
alter table events add column if not exists confirm_email_message text;
