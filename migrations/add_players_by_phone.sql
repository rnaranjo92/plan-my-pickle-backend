-- pmp_player_ids_by_phone — find player rows whose phone matches, IGNORING
-- formatting.
--
-- Why: `players.phone` and `pmp_profiles.phone` both store the number exactly as
-- a human typed it, and the two humans are different people (the organizer vs
-- the player). So "(619) 555-0100", "619-555-0100" and "6195550100" all coexist
-- for the same person, and every `phone=eq.<raw>` comparison in the codebase
-- silently fails to match them. That is why a guest registration made by phone
-- never attached itself to the player's account.
--
-- Comparing on the LAST 10 DIGITS makes country-code and punctuation differences
-- irrelevant, which is the only comparison that survives real-world entry.
--
-- p_unlinked_only restricts to guest rows (user_id is null) — what the signup
-- linker claims, since a row already tied to an account belongs to someone else.
--
-- SECURITY: no auth tables here, but it still exposes player ids by phone, so
-- execute is service_role only (the backend), never anon/authenticated.
create or replace function public.pmp_player_ids_by_phone(
  p_phone          text,
  p_unlinked_only  boolean default false
)
returns uuid[]
language sql
stable
security definer
set search_path = public
as $$
  select coalesce(array_agg(p.id), '{}'::uuid[])
  from players p
  where length(right(regexp_replace(coalesce(p_phone, ''), '\D', '', 'g'), 10)) = 10
    and right(regexp_replace(coalesce(p.phone, ''), '\D', '', 'g'), 10)
      = right(regexp_replace(p_phone, '\D', '', 'g'), 10)
    and (not p_unlinked_only or p.user_id is null);
$$;

revoke all on function public.pmp_player_ids_by_phone(text, boolean) from public;
revoke all on function public.pmp_player_ids_by_phone(text, boolean) from anon;
revoke all on function public.pmp_player_ids_by_phone(text, boolean) from authenticated;
grant execute on function public.pmp_player_ids_by_phone(text, boolean) to service_role;
