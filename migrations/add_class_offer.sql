-- Paid-waitlist "claim window": when a seat frees up in a PAID class, the next
-- waitlister is moved to status 'offered' (holds the seat) with a deadline to
-- confirm & pay, instead of being silently enrolled/charged. Expired offers roll
-- to the next person.
alter table coaching_enrollments add column if not exists offer_expires_at timestamptz;
