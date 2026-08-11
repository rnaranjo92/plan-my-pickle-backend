-- pmp_account_by_contact — resolve a typed email/phone to an ACCOUNT id.
--
-- Why this exists: when an organizer adds a player by hand, the backend tries to
-- tie the registration to that person's account so the league shows up in their
-- app. It used to search only the `players` table, which meant:
--   * an account that had never registered for an event was unfindable by email
--     (auth.users is the authoritative store and was never consulted), and
--   * phones were compared with raw string equality, so "(619) 555-0100" never
--     matched "6195550100".
--
-- The NAME is deliberately not part of matching. Organizers type nicknames and
-- misspellings ("Kay" for an account named "Kim Naranji"), so requiring a name
-- match silently broke the link. Email and phone are identifiers; a name is not.
--
-- SECURITY: reads auth.users, so it is SECURITY DEFINER and execute is granted
-- ONLY to service_role (the backend). Left callable by anon/authenticated it
-- would be an account-enumeration oracle.
create or replace function public.pmp_account_by_contact(
  p_email text default null,
  p_phone text default null
)
returns uuid
language plpgsql
security definer
set search_path = public, auth
as $$
declare
  v_id     uuid;
  v_digits text;
  v_count  int;
begin
  -- 1) EMAIL is exact and unique in auth.users → the most confident match.
  if p_email is not null and btrim(p_email) <> '' then
    select id into v_id
    from auth.users
    where lower(email) = lower(btrim(p_email))
    limit 1;
    if v_id is not null then
      return v_id;
    end if;
  end if;

  -- 2) PHONE, compared on the last 10 digits so formatting never matters.
  --    Only link when EXACTLY ONE account matches: a shared household number
  --    must not silently attach a registration to the wrong spouse.
  v_digits := right(regexp_replace(coalesce(p_phone, ''), '\D', '', 'g'), 10);
  if length(v_digits) = 10 then
    -- 2a) auth.users.phone
    select count(*) into v_count
    from auth.users
    where right(regexp_replace(coalesce(phone, ''), '\D', '', 'g'), 10) = v_digits;
    if v_count = 1 then
      select id into v_id
      from auth.users
      where right(regexp_replace(coalesce(phone, ''), '\D', '', 'g'), 10) = v_digits
      limit 1;
      return v_id;
    end if;

    -- 2b) pmp_profiles.phone — where most accounts actually keep their number
    --     (auth.users.phone is only set for SMS-OTP signups).
    select count(distinct user_id) into v_count
    from pmp_profiles
    where right(regexp_replace(coalesce(phone, ''), '\D', '', 'g'), 10) = v_digits;
    if v_count = 1 then
      select user_id into v_id
      from pmp_profiles
      where right(regexp_replace(coalesce(phone, ''), '\D', '', 'g'), 10) = v_digits
      limit 1;
      return v_id;
    end if;

    -- 2c) an account-linked players row carrying that number.
    select count(distinct user_id) into v_count
    from players
    where user_id is not null
      and right(regexp_replace(coalesce(phone, ''), '\D', '', 'g'), 10) = v_digits;
    if v_count = 1 then
      select user_id into v_id
      from players
      where user_id is not null
        and right(regexp_replace(coalesce(phone, ''), '\D', '', 'g'), 10) = v_digits
      limit 1;
      return v_id;
    end if;
  end if;

  return null;
end;
$$;

revoke all on function public.pmp_account_by_contact(text, text) from public;
revoke all on function public.pmp_account_by_contact(text, text) from anon;
revoke all on function public.pmp_account_by_contact(text, text) from authenticated;
grant execute on function public.pmp_account_by_contact(text, text) to service_role;
