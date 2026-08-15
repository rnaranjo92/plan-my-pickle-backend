-- The coach subscription, independent of organizer Premium.
--
-- Premium and the coach plan are DIFFERENT products bought by DIFFERENT people:
-- an organizer running tournaments and a coach teaching lessons share almost no
-- reason to pay. Reusing the `premium` boolean for both would mean a coach
-- subscribing silently gains organizer Premium, and cancelling either plan
-- revokes the other — one flag cannot represent two independent subscriptions.
alter table pmp_profiles add column if not exists coach_plan boolean not null default false;

-- The coach plan's own Stripe subscription id. Separate from
-- stripe_subscription_id (Premium's) so a coach can hold both plans and manage
-- or cancel each on its own.
alter table pmp_profiles add column if not exists coach_subscription_id text;
alter table pmp_profiles add column if not exists coach_subscription_status text;

create index if not exists pmp_profiles_coach_sub_idx
  on pmp_profiles (coach_subscription_id) where coach_subscription_id is not null;

comment on column pmp_profiles.coach_plan is
  'Paid coach subscription is active. Independent of premium (organizer plan).';
