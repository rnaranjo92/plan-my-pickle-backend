-- Manual Venmo payment handle (like zelle_handle) — organizers collect the entry
-- fee via a tap-to-pay Venmo link while the marketplace gateway is still pending.
alter table events add column if not exists venmo_handle text;
