-- Persist the one-time first-run onboarding flag SERVER-SIDE so it survives a
-- browser cache clear and follows the account to a new device — instead of only
-- a device-local SharedPreferences flag that a cache clear wipes (which re-nagged
-- the "See tournaments near you" step on every clear).
alter table pmp_profiles
  add column if not exists onboarded boolean not null default false;

-- Backfill: any account that already has a saved location/city (i.e. has clearly
-- been through onboarding) is marked onboarded so existing users aren't re-asked.
update pmp_profiles
set onboarded = true
where onboarded = false
  and coalesce(btrim(city), '') <> '';
