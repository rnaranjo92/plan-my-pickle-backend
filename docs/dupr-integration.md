# DUPR integration — status, setup, and review-submission package

PlanMyPickle is an approved DUPR API partner (UAT keys issued 2026-06-22).
This doc tracks setup, what's implemented vs. the requirements, and the package
to email `tech@mydupr.com` to request production keys.

## Environment (Railway — `plan-my-pickle-backend`)
Required to go live (UAT defaults shown):
- `DUPR_CLIENT_KEY`, `DUPR_CLIENT_SECRET` — partner credentials (set ✅)
- `DUPR_CLUB_ID` = `6364521321` (UAT) — matches submit as a club match (set ✅)
- `DUPR_WEBHOOK_SECRET` — **REQUIRED for rating webhooks.** Any long random
  string. The webhook receiver is fail-closed: it rejects until this is set, and
  we register the webhook URL with it as `?token=`. **TODO: set this.**
- Optional: `DUPR_BASE_URL` (prod `https://api.dupr.com/api`), `DUPR_SSO_BASE`
  (prod `https://dashboard.dupr.com`), `DUPR_API_VERSION` (default `v1.0`),
  `DUPR_CLIENT_ID`, `DUPR_WEBHOOK_URL`.

## Migrations (run in Supabase SQL editor)
- `add_dupr_connections.sql` — DUPR account links (✅ run)
- `add_dupr_connections_unique.sql` — unique `dupr_id` (**TODO: run**)

## Requirements vs. implementation
1. **SSO is the only way to connect (no manual DUPR id)** — ✅ Done. The DUPR-id
   text field was removed; users connect via the DUPR SSO iframe
   (Profile → Connect DUPR account; also prompted on sanctioned-event
   registration). The id is stored from the SSO postMessage, never typed.
2. **Rating visible + kept fresh via webhooks** — ✅ Done. The Profile card shows
   the connected rating; on connect we subscribe the user to RATING events
   (seed delivers current rating); the `POST /dupr/webhook` receiver updates it.
   (Requires `DUPR_WEBHOOK_SECRET`.)
3. **User entitlement gating** — ⚠️ Partial. Match submission relies on DUPR's
   own BASIC_L1 enforcement (DUPR rejects ineligible players); platform access
   isn't DUPR-entitlement-gated. Confirm with DUPR whether this is sufficient.
4. **Match create / update / delete, owner-only** — ⚠️ Create ✅ (owner-only:
   the organizer flushes results via "Import to DUPR" → `SubmitPendingToDupr`;
   players can't submit). **Update + delete NOT yet implemented** — a corrected
   or deleted score does not propagate to DUPR. **This is the main gate before
   submitting for review.**

## Architecture notes
- One submission path: scoring queues a row (`dupr_submissions`); the organizer
  flushes pending → DUPR via the owner-only "Import to DUPR" button. Idempotent
  per match (unique `identifier` = match id). Missing-DUPR-id players and byes
  are reported/skipped, not silently dropped.
- Webhook receiver is fail-closed on `DUPR_WEBHOOK_SECRET`; only updates an
  already-connected `dupr_id`.

## Remaining before requesting production keys
1. **Build match update + delete** (req #4) — gateway UpdateMatch/DeleteMatch
   (using the stored `dupr_submissions.provider_ref`), call UpdateMatch when an
   already-submitted match is re-scored, DeleteMatch when a match is deleted.
2. Set `DUPR_WEBHOOK_SECRET` + run `add_dupr_connections_unique.sql`.
3. (Hardening) verify the SSO userToken actually owns the submitted duprId on
   connect; derive `matchType` from event scoring (currently SIDEOUT).

## Review-submission email (draft — send to tech@mydupr.com, subject incl. Client ID 7673785325)
> Platform: https://app.planmypickle.com (organizer flow on the web app).
> Test accounts: our UAT logins + the DUPR UAT players (player1–4@planmypickle.com).
> Compliance:
> 1. SSO-only connect — Profile → Connect DUPR account → DUPR SSO iframe; no
>    manual id entry anywhere. (Demo: connect player1.)
> 2. Rating shown + webhook-updated — connected rating shows on Profile; we
>    subscribe to RATING events on connect.
> 3. Entitlements — [confirm interpretation with DUPR].
> 4. Match create/update/delete — organizer "Import to DUPR" submits a sanctioned
>    event's results (owner-only); update/delete via [pending].
