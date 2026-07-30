-- Coach cancellation policy: sets refund/cancel expectations and a cutoff window
-- before a class/session within which players can no longer cancel.
-- Values: flexible (anytime), moderate (24h), strict (72h). Default flexible.
alter table coach_profiles add column if not exists cancel_policy text not null default 'flexible';
