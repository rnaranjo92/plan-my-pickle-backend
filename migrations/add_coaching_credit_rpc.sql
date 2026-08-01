-- Atomic class-credit balance mutations. The Go read-then-write (cur -> cur±1)
-- can lose an update when two different seats of the SAME player settle
-- concurrently; these do the increment/decrement in one statement so the balance
-- is always correct. The service calls them via /rest/v1/rpc and falls back to
-- the old read-then-write if they're not present yet.

-- Add one credit back (used when a credit-funded seat is refunded).
create or replace function restore_coach_credit(p_coach uuid, p_user uuid)
returns void
language sql
as $$
  insert into coaching_credits (coach_id, user_id, credits_remaining, updated_at)
    values (p_coach, p_user, 1, now())
  on conflict (coach_id, user_id)
    do update set credits_remaining = coaching_credits.credits_remaining + 1,
                  updated_at = now();
$$;

-- Spend one credit iff the player has one. Returns true when a credit was spent.
create or replace function spend_coach_credit(p_coach uuid, p_user uuid)
returns boolean
language plpgsql
as $$
declare
  affected int;
begin
  update coaching_credits
    set credits_remaining = credits_remaining - 1, updated_at = now()
    where coach_id = p_coach and user_id = p_user and credits_remaining > 0;
  get diagnostics affected = row_count;
  return affected > 0;
end;
$$;
