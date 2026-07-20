-- Public league discovery (opt-in). `listed` lets an organizer publish a league
-- to the public "pickleball leagues in <city>" SEO hubs; default FALSE keeps
-- every existing and new league private unless the owner opts in. The league's
-- city/state is derived from its events (sessions), not stored here. Additive +
-- default false, so deploying the code before this migration is safe (the backend
-- only writes/reads `listed` once a probe confirms the column exists).
alter table leagues add column if not exists listed boolean not null default false;
create index if not exists leagues_listed_idx on leagues(listed) where listed;
