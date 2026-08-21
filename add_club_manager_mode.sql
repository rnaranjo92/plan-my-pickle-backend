-- Club manager mode: swaps the app's two player tabs (Play, Coach) for one
-- Manage tab. Stored per ACCOUNT so it survives sign-out and follows the owner
-- to another device -- a device-local setting would revert them every time.
--
-- Safe to re-run.
alter table pmp_profiles
  add column if not exists club_manager_mode boolean not null default false;
