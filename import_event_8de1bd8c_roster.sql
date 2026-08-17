-- Import roster into event 8de1bd8c-79e2-41b5-be72-7b775693264d
-- Two divisions (High Intermediate, Intermediate), 6 doubles teams each.
--
-- Model: a doubles team = two guest `players` rows + two `registrations` rows
-- that reference each other via partner_id (same bracket). Divisions are
-- `brackets` rows. IDs auto-generate.
--
-- Safe to run in Supabase → SQL Editor. Idempotent:
--   * reuses a division that already exists on this event (matched by name)
--   * skips any team whose first player is already registered in that division
-- so a second run won't create duplicates.

do $$
declare
  v_event   uuid := '8de1bd8c-79e2-41b5-be72-7b775693264d';
  -- Division type for these doubles brackets. Change to 'open' or
  -- 'mixed_doubles' if this isn't a women's event. (Only affects internal
  -- pairing/eligibility — the visible label is the bracket NAME below.)
  v_divtype text := 'womens_doubles';

  v_bracket uuid;
  v_pa      uuid;
  v_pb      uuid;
  d int;
  i int;

  div_names text[] := array['High Intermediate','Intermediate'];

  -- Teams, one array per division (partnerA[i] plays with partnerB[i]).
  hi_a text[] := array[
    'Kay Naranjo','Theresa Mendoza','Jocelyn Almeria',
    'Concon Mendoza','Ayana Pitterson','Angie Guiterrez'];
  hi_b text[] := array[
    'Marissa Bejarano-Stevenson','Milli Ellis','Norma Zavala',
    'Sharie Soriano','Mari Nevarez','Cynthia Chenier'];

  int_a text[] := array[
    'Carolyn Factuar','Shauna Valerio','Julie Peck',
    'Celina Ali','Tiare Rice','Marjorie Barrero'];
  int_b text[] := array[
    'Mary Davis','Ashley C','Antonia Sueoka',
    'Kelly Christensen','Christine Arellano','Shelley Gurtiza'];

  pa text[];
  pb text[];
begin
  for d in 1..array_length(div_names, 1) loop
    if d = 1 then pa := hi_a;  pb := hi_b;
    else          pa := int_a; pb := int_b;
    end if;

    -- Reuse the division if it already exists on this event, else create it.
    select id into v_bracket
      from brackets
      where event_id = v_event and lower(name) = lower(div_names[d])
      limit 1;
    if v_bracket is null then
      insert into brackets (event_id, name, division_type, sort_order)
        values (v_event, div_names[d], v_divtype, d - 1)
        returning id into v_bracket;
    end if;

    for i in 1..array_length(pa, 1) loop
      -- Skip a team already imported into this division (idempotency).
      if exists (
        select 1
          from registrations r
          join players p on p.id = r.player_id
         where r.bracket_id = v_bracket
           and lower(p.full_name) = lower(pa[i])
      ) then
        continue;
      end if;

      insert into players (full_name) values (pa[i]) returning id into v_pa;
      insert into players (full_name) values (pb[i]) returning id into v_pb;

      -- Two mutually-linked registrations = one doubles team.
      insert into registrations
        (event_id, player_id, partner_id, bracket_id, check_in_token, approved)
        values (v_event, v_pa, v_pb, v_bracket, gen_random_uuid()::text, true);
      insert into registrations
        (event_id, player_id, partner_id, bracket_id, check_in_token, approved)
        values (v_event, v_pb, v_pa, v_bracket, gen_random_uuid()::text, true);
    end loop;
  end loop;
end $$;

-- Verify: should show 12 registrations in each division (6 teams x 2 players).
select b.name as division, count(r.id) as registrations
  from brackets b
  left join registrations r on r.bracket_id = b.id
 where b.event_id = '8de1bd8c-79e2-41b5-be72-7b775693264d'
   and b.name in ('High Intermediate','Intermediate')
 group by b.name
 order by b.name;
