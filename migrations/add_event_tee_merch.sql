-- Custom event-tee presale — enrich the (dormant) event-tee add-on into a real
-- merch item: a name, front/back design images, and the sizes the organizer
-- offers. Registrants pick a size; the tee rides the EXISTING entry-fee charge
-- (Stripe Connect / Zelle / organizer mark-paid) via registrationChargeCents.
-- The organizer gets a size-breakdown fulfillment report for their printer.
--
-- Note: `addon_tee_cents` already exists on events and `addon_tee` on
-- registrations (the original add-on rail) — this only adds the new columns.
alter table events
  add column if not exists addon_tee_name      text,
  add column if not exists addon_tee_front_url text,
  add column if not exists addon_tee_back_url  text,
  add column if not exists addon_tee_sizes     text[];

alter table registrations
  add column if not exists addon_tee_size text;
