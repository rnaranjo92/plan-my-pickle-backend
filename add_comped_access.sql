-- Comped access as DATA, separate from billing.
--
-- Comps were being recorded by setting pmp_profiles.premium = true by hand.
-- That column is owned by Stripe: applySubscriptionEvent overwrites it on every
-- subscription webhook, and a reconciler that asks Stripe "who is actually
-- subscribed?" would revoke every comped account, because none of them are.
-- A tester losing access is bad; losing them silently, with no record of who
-- granted it or why, is worse.
--
-- So comps get their own column. Billing never writes it, the reconciler never
-- reads it, and every comp carries a reason and a date so the list can be
-- reviewed instead of archaeologically reconstructed.
alter table pmp_profiles add column if not exists comped boolean not null default false;

-- Why, who, and when. Free text on purpose — "Life Time tester", "founding
-- organizer", "beta feedback" are the answers that matter, and a rigid enum
-- would just be bypassed.
alter table pmp_profiles add column if not exists comp_reason text;
alter table pmp_profiles add column if not exists comped_at timestamptz;
alter table pmp_profiles add column if not exists comped_by text;

-- Comped accounts are a small set that gets listed in full, so a partial index
-- keeps that scan cheap forever.
create index if not exists pmp_profiles_comped_idx
  on pmp_profiles (comped) where comped = true;

comment on column pmp_profiles.comped is
  'Manually granted access. Independent of premium (Stripe-owned) — billing must never write this.';

-- Adopt the comps that were set by hand on `premium`.
--
-- Anyone holding premium with no Stripe subscription id was comped manually —
-- a real subscriber always has one. This is the migration that stops the
-- reconciler from revoking them.
update pmp_profiles
   set comped = true,
       comp_reason = coalesce(comp_reason, 'adopted from manual premium grant'),
       comped_at = coalesce(comped_at, now())
 where premium = true
   and coalesce(stripe_subscription_id, '') = '';
