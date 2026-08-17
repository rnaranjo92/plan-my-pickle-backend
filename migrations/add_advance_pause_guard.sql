-- A paused rotation session must not be advanced out from under the organizer.
--
-- THE BUG: advance_rotation_session() force-writes status='live' as part of
-- moving to the next round. The Go layer checks for a pause before it starts
-- seeding the next round's courts, but that work takes long enough that an
-- organizer who taps Pause DURING it has their pause silently reverted — the
-- clock restarts and the room rotates out of a game nobody finished. Pausing
-- exists precisely to stop that, so the last word has to belong to the database.
--
-- WHY A TRIGGER AND NOT A REWRITE OF THE RPC: the guard has to sit below
-- whatever the function does, and a trigger adds it without touching (or
-- needing to know) the function body. Nothing existing is replaced, so this is
-- safe to run against the live database mid-season.
--
-- WHAT IT BLOCKS: exactly one transition — leaving 'paused' for 'live' while
-- ALSO bumping the round. That is only ever an advance racing a pause.
--   * Resume (paused -> live, same round) still works.
--   * A normal advance (live -> live, round + 1) still works.
--   * End, and every other status write, are untouched.
--
-- Idempotent: safe to re-run.
--
-- HOW TO APPLY: paste into the Supabase SQL editor and run.

create or replace function rotation_block_paused_advance()
returns trigger
language plpgsql
as $$
begin
  if old.status = 'paused'
     and new.status = 'live'
     and new.current_round is distinct from old.current_round then
    raise exception
      'rotation session % is paused — resume it before starting round %',
      old.id, new.current_round
      using errcode = 'check_violation';
  end if;
  return new;
end;
$$;

drop trigger if exists rotation_block_paused_advance on rotation_sessions;

create trigger rotation_block_paused_advance
  before update on rotation_sessions
  for each row
  execute function rotation_block_paused_advance();

-- Verify (should list one trigger):
--   select tgname from pg_trigger
--   where tgrelid = 'rotation_sessions'::regclass and not tgisinternal;
