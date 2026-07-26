-- Phone OTP verification — a one-time 6-digit SMS code a new account verifies to
-- become "fully registered" (anti-abuse). Enforcement is gated on the backend
-- SIGNUP_OTP_REQUIRED env flag; this migration just adds the storage.

-- Whether the account has verified its phone via SMS OTP.
alter table pmp_profiles
  add column if not exists phone_verified boolean not null default false;

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
