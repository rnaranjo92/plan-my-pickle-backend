-- Phone OTP verification — a one-time 6-digit SMS code a new account verifies to
-- become "fully registered" (anti-abuse). Enforcement is gated on the backend
-- SIGNUP_OTP_REQUIRED env flag; this migration just adds the storage.

-- Whether the account has verified its phone via SMS OTP.
alter table pmp_profiles
  add column if not exists phone_verified boolean not null default false;

-- GRANDFATHER existing accounts: everyone who already signed up is trusted, so
-- turning the gate on must NOT lock them out. Only NEW rows (future sign-ups)
-- default to false and must verify.
-- ⚠️ RUN ONCE ONLY — this has no time bound, so RE-RUNNING it would wrongly
--    grandfather every account that signed up (unverified) since the first run.
--    Already applied in prod 2026-07-26. Do NOT run again.
update pmp_profiles set phone_verified = true where phone_verified = false;

-- pmp_profiles rows are created LAZILY (no signup trigger) — a long-time
-- organizer who never edited their profile has NO row, and the UPDATE above can't
-- reach them. Create a verified row for every existing auth user that lacks one,
-- so the gate never locks out a pre-existing account. (If this INSERT errors on a
-- NOT-NULL column, add that column to the select list.)
insert into pmp_profiles (user_id, phone_verified)
select u.id, true
from auth.users u
where not exists (select 1 from pmp_profiles p where p.user_id = u.id)
on conflict (user_id) do nothing;

-- One pending code per user (upserted on resend). Codes are stored HASHED
-- (sha256 of secret|user_id|code) — never plaintext.
create table if not exists phone_otps (
  user_id      uuid primary key,
  phone        text        not null,
  code_hash    text        not null,
  expires_at   timestamptz not null,
  attempts     int         not null default 0,
  last_sent_at timestamptz not null default now()
);
