-- Rotation ladder: substitutions.
--
-- Someone's knee goes at round 5 and a friend takes over. The scores already on
-- the board are the first player's and must stay theirs; every round from here
-- belongs to whoever actually played it. So a substitution is NOT an edit of the
-- outgoing player's name -- renaming would silently hand one person's night to
-- another. It creates a second roster row and swaps the SEAT.
--
-- That also makes a chain (A -> B -> C -> D) fall out for free: the substitute is
-- an ordinary roster player, so subbing them out again is the same operation, and
-- the chain is just these rows in round order.
create table if not exists rotation_substitutions (
  id         uuid primary key default gen_random_uuid(),
  session_id uuid not null references rotation_sessions(id) on delete cascade,
  -- The round the swap took effect: the outgoing player owns everything before
  -- it, the incoming player everything from it on.
  round      int  not null,
  out_player uuid not null references rotation_players(id) on delete cascade,
  in_player  uuid not null references rotation_players(id) on delete cascade,
  created_at timestamptz not null default now()
);

create index if not exists idx_rotation_subs_session
  on rotation_substitutions (session_id, round);
