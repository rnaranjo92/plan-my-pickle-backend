-- Rotation ladder: make a substitution ONE atomic operation.
--
-- Handing a seat to a substitute touches five things: a new roster row, four
-- court-seat columns, the waiting queue, the history record, and retiring the
-- player who left. Done as five separate calls from Go, every gap between them
-- is a way to end the night with a broken room:
--
--   * a failure midway leaves an ACTIVE, unseated substitute, whom the next
--     advance sweeps onto a court -- a fifth body in a four-person room, and
--     unremovable, because the roster is locked once a session is live;
--   * a double-tapped button passes the "is she still active?" check twice and
--     inserts two substitutes;
--   * the queue was rewritten from a snapshot read several round-trips earlier,
--     so an advance landing in between had its queue clobbered -- erasing the
--     real resting players and putting one person on two courts at once.
--
-- Inside one function, under the session's row lock, none of those windows
-- exist. This is the same reason start_rotation_session and rotation_join_bench
-- are RPCs rather than Go-side read-modify-writes.
--
-- Also fixes WHICH ROUND the swap takes effect. The outgoing player keeps the
-- rounds they played; if their score for the current round is already on the
-- board, they finished it, so the substitute starts from the NEXT round. Taking
-- their seat in a round they had already scored orphaned those scores from the
-- court, which made the court unresolvable -- and an unresolvable court silently
-- defaults to "team A won", promoting the pair that actually lost.

-- One substitution per departing player. The backstop for a double-tap: the
-- second call fails here instead of quietly building a second substitute.
create unique index if not exists idx_rotation_subs_one_per_out
  on rotation_substitutions (session_id, out_player);

-- Index the FK columns. Deleting a session cascades to every rotation_players
-- row, and each of those scans this table looking for references.
create index if not exists idx_rotation_subs_out on rotation_substitutions (out_player);
create index if not exists idx_rotation_subs_in  on rotation_substitutions (in_player);

-- Service-role only (the Go backend); no anon/authenticated policy, matching the
-- other rotation tables. Without this, anyone holding the app's published anon
-- key could delete a session's substitution history or fabricate rows -- and
-- these rows are what decide whose points are whose.
alter table rotation_substitutions enable row level security;

-- start_court is likewise service-role only.
alter table rotation_players enable row level security;

create or replace function rotation_substitute(
  p_session uuid,
  p_out     uuid,
  p_name    text,
  p_rating  numeric,
  p_entrant uuid
) returns jsonb
language plpgsql
as $$
declare
  v_status text;
  v_round  int;
  v_in     uuid;
  v_eff    int;
  v_scored boolean;
  v_row    jsonb;
begin
  -- Lock the session for the whole operation: an advance racing this can no
  -- longer interleave with it.
  select status, coalesce(current_round, 0)
    into v_status, v_round
    from rotation_sessions
   where id = p_session
     for update;

  if not found then
    return jsonb_build_object('ok', false, 'reason', 'no_session');
  end if;
  if v_status = 'setup' then
    return jsonb_build_object('ok', false, 'reason', 'not_started');
  end if;
  if v_status not in ('live', 'paused') then
    return jsonb_build_object('ok', false, 'reason', 'finished');
  end if;
  if v_round < 1 then
    v_round := 1;
  end if;

  if not exists (select 1 from rotation_players
                  where id = p_out and session_id = p_session and active) then
    return jsonb_build_object('ok', false, 'reason', 'out_not_active');
  end if;
  if exists (select 1 from rotation_substitutions
              where session_id = p_session and out_player = p_out) then
    return jsonb_build_object('ok', false, 'reason', 'already_substituted');
  end if;

  -- Which round does the substitute inherit? If the outgoing player's score for
  -- the live round is already recorded, they finished that round and it stays
  -- theirs; the substitute starts from the next one.
  select exists (select 1 from rotation_round_scores
                  where session_id = p_session
                    and round = v_round
                    and rotation_player_id = p_out
                    and score is not null)
    into v_scored;
  v_eff := case when v_scored then v_round + 1 else v_round end;

  insert into rotation_players (session_id, display_name, self_rating, entrant_id, active)
       values (p_session, p_name, p_rating, p_entrant, true)
    returning id into v_in;

  -- Take the seat from the effective round on. >= rather than = so a round that
  -- has already been written ahead is covered too.
  update rotation_round_courts set team_a_p1 = v_in
   where session_id = p_session and round >= v_eff and team_a_p1 = p_out;
  update rotation_round_courts set team_a_p2 = v_in
   where session_id = p_session and round >= v_eff and team_a_p2 = p_out;
  update rotation_round_courts set team_b_p1 = v_in
   where session_id = p_session and round >= v_eff and team_b_p1 = p_out;
  update rotation_round_courts set team_b_p2 = v_in
   where session_id = p_session and round >= v_eff and team_b_p2 = p_out;

  -- If they were waiting rather than playing, the substitute inherits their
  -- PLACE in the queue, not the back of it. The column is an ordered id list;
  -- it may be jsonb or text[] depending on when the table was made, so the swap
  -- is issued dynamically -- the unreached branch is never parsed.
  if exists (select 1 from information_schema.columns
              where table_name = 'rotation_sessions'
                and column_name = 'bench'
                and data_type = 'ARRAY') then
    execute 'update rotation_sessions
                set bench = array_replace(bench, $2, $3)
              where id = $1'
      using p_session, p_out::text, v_in::text;
  else
    execute 'update rotation_sessions
                set bench = coalesce((
                      select jsonb_agg(case when x = $2 then $3 else x end)
                        from jsonb_array_elements_text(bench::jsonb) x), ''[]''::jsonb)
              where id = $1'
      using p_session, p_out::text, v_in::text;
  end if;

  insert into rotation_substitutions (session_id, round, out_player, in_player)
       values (p_session, v_eff, p_out, v_in);

  update rotation_players set active = false where id = p_out;

  select to_jsonb(p) into v_row from rotation_players p where p.id = v_in;
  return jsonb_build_object('ok', true, 'round', v_eff, 'player', v_row);
end;
$$;

-- Verify — expect ok=false / no_session (a substitution against a session that
-- doesn't exist), which proves the function is installed and callable.
select rotation_substitute(
  '00000000-0000-0000-0000-000000000000'::uuid,
  '00000000-0000-0000-0000-000000000000'::uuid,
  'probe', 3.0, null) as probe;
