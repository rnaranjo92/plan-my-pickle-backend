-- The Club plan: the $59/mo tier, as its OWN columns.
--
-- Its own columns rather than reusing `premium`, for the same reason the coach
-- plan has its own: the plans are separate products bought by different people,
-- Stripe webhooks are routed to columns BY PRICE ID, and sharing a column is
-- how cancelling one plan revokes the other.
--
-- The plan is held by the club OWNER's account and covers every club they own —
-- "$59/mo for your whole club" — which is the same owner-entitlement rule the
-- season archive and coach seats already read.
--
-- SHIPS INERT twice over: nothing reads these columns until the code deploys,
-- and the code grants nothing until STRIPE_CLUB_PRICE_ID is set in Railway —
-- creating that price is the act that turns the tier on.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run. Idempotent.

alter table pmp_profiles add column if not exists club_plan boolean not null default false;
alter table pmp_profiles add column if not exists club_subscription_status text;
alter table pmp_profiles add column if not exists club_subscription_id text;
-- Shared with Premium and the coach plan, and written by the club webhook too.
-- It already exists wherever the Premium rails were set up, but no migration in
-- this repo creates it — so claim it here rather than assume. Without it the
-- club upsert 400s, the webhook 500s on every retry, and a paid subscription
-- never records.
alter table pmp_profiles add column if not exists stripe_customer_id text;

-- Verify:
--   select count(*) from pmp_profiles where club_plan;
