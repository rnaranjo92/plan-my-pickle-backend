-- A DUPR account links to at most ONE app user. Prevents two users connecting
-- (claiming) the same DUPR id — which would fan a rating webhook out to multiple
-- accounts and enable claiming someone else's rating. Run AFTER
-- add_dupr_connections.sql. (Fails if a duplicate dupr_id already exists — dedupe
-- first if so; in practice there should be none.)
create unique index if not exists dupr_connections_dupr_id_unique
  on dupr_connections (dupr_id);
drop index if exists dupr_connections_dupr_id_idx;
