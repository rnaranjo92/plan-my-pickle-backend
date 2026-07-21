-- Manual GCash collection for Philippine events: the organizer's GCash mobile
-- number, shown to registrants so they can pay out-of-band; the organizer then
-- marks the registration paid (same flow as the existing Zelle/Venmo handles).
-- Read/write is columnReady-guarded in the service, so pre-migration it's inert.
alter table events add column if not exists gcash_handle text;
