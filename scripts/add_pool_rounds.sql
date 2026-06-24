-- Min/max bounds on the pool-play round-robin length (0 = unset).
-- max_pool_rounds caps a full round-robin (a partial RR for big pools);
-- min_pool_rounds tops it up by repeating matchups so everyone gets a
-- guaranteed number of games. Applied in engine.GenerateSchedule.
alter table events add column if not exists min_pool_rounds int not null default 0;
alter table events add column if not exists max_pool_rounds int not null default 0;
