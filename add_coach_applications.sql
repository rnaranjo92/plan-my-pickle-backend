-- Coaches applying to teach on PlanMyPickle.
--
-- Until now a coach had to reach the owner privately and be added to the
-- `instructors` allowlist by hand. That gate works — nobody self-serves into the
-- directory — but there was no way IN: no page to apply on, and no record of who
-- asked. This is the funnel in front of the gate, not a replacement for it.
--
-- Approving an application is what writes the `instructors` row, so the existing
-- allowlist stays the single thing that grants coach access.
create table if not exists coach_applications (
  id uuid primary key default gen_random_uuid(),
  -- Contact. Email is the key the allowlist is keyed on, so it's required and
  -- stored lower-cased.
  email text not null,
  name text not null,
  phone text,
  city text,
  -- What they teach and what backs it up. Free text on purpose: a certification
  -- number, a club, a coaching history — anything the reviewer can check. A
  -- rigid schema here would reject the honest applicant with an unusual answer.
  certifications text,
  experience text,
  -- Insurance is the cheapest risk transfer available, so it's asked for
  -- explicitly rather than buried in free text.
  has_insurance boolean not null default false,
  note text,
  -- pending | approved | rejected
  status text not null default 'pending',
  -- Why it was rejected, for the applicant and for anyone reviewing later.
  decision_note text,
  decided_at timestamptz,
  decided_by text,
  created_at timestamptz not null default now()
);

create index if not exists coach_applications_status_idx
  on coach_applications (status, created_at desc);

-- One PENDING application per email. A coach who applies twice while waiting
-- shouldn't create two rows for the reviewer to reconcile, but a REJECTED
-- applicant must be able to apply again later with better credentials.
create unique index if not exists coach_applications_pending_email_idx
  on coach_applications (lower(email)) where status = 'pending';

comment on table coach_applications is
  'Coaches applying to teach. Approving writes the instructors allowlist row.';
