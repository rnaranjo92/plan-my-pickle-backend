# PlanMyPickle — Backend API (Go) 🥒

The decoupled backend for PlanMyPickle. It owns the data + tournament logic and
exposes a small JSON REST API. The Flutter app (and later the web/desktop
clients) talk to **this** instead of a local database.

- **Go** (stdlib `net/http`, Go 1.22+ routing) — one external dep.
- **SQLite** via `modernc.org/sqlite` (pure Go, no cgo) — runs anywhere, reuses
  the exact `0001_initial_schema.sql`. **Postgres-portable** for production.
- Tournament engines (round-robin + single-elimination) are **`go test`-verified**.

## Layout
```
cmd/api/main.go              # server entrypoint
internal/engine/             # round-robin + single-elim (pure logic, tested)
internal/store/              # sqlite open + migration (embedded)
internal/model/              # API DTOs
internal/service/            # business operations (create/register/schedule/score/standings)
internal/api/                # HTTP handlers + routing + CORS
```

## Run
```bash
cd plan-my-pickle-backend
go test ./...          # engines + store + service (end-to-end) all pass
go run ./cmd/api       # serves on :8080  (PMP_ADDR, PMP_DSN to override)
```
`PMP_DSN` defaults to `file:planmypickle.db` (on-disk). Use
`file:pmp?mode=memory&cache=shared` for an ephemeral dev DB.

## API
| Method & path | Purpose |
|---|---|
| `GET /healthz` | health check |
| `POST /events` | create a tournament (format, brackets, DUPR, courts, fee) |
| `GET /events` · `GET /events/{id}` | list / fetch events |
| `GET /events/{id}/brackets` | rating divisions |
| `POST /events/{id}/register` | register a player (auto-assigned to a bracket) |
| `GET /events/{id}/registrations` | roster |
| `POST /events/{id}/schedule` | generate the schedule per the event's format → `{matches}` |
| `GET /events/{id}/standings?by=wins\|points&bracketId=` | live leaderboard |
| `POST /matches/{id}/score` | record `{team1Score,team2Score}` (advances the bracket) |
| `POST /brackets/{id}/playoff?topN=` | seed a playoff bracket from pool standings |
| `GET /brackets/{id}/matches` | the live bracket dashboard data (with player names) |
| `GET /events/{id}/rounds` · `GET /rounds/{id}/matches` | pool schedule (rounds → matches w/ names) |
| `POST /registrations/{id}/pay` | collect the fee `{provider}` (Stripe/PayPal/Venmo/manual) |
| `POST /registrations/{id}/checkin` · `POST /events/{id}/checkin` | check in (manual / by QR `{token}`) |
| `POST /rounds/{id}/start` | activate a round + text players their court → `{sent}` |
| `POST /events/{id}/dupr/import` | submit sanctioned results to DUPR → `{submitted,failed}` |
| `POST /events/{id}/verify-admin` | gate the coordinator scoring page `{code}` → `{ok}` |

### Example
```bash
curl -s localhost:8080/events -d '{"name":"Friday Open","format":"singles","tournamentFormat":"single_elim","numCourts":2}'
# -> {"id":"..."}
curl -s localhost:8080/events/$ID/register -d '{"fullName":"Ana","skillLevel":3.9}'
curl -s -XPOST localhost:8080/events/$ID/schedule
curl -s -XPOST localhost:8080/matches/$MATCH/score -d '{"team1Score":11,"team2Score":6}'
curl -s "localhost:8080/events/$ID/standings?by=wins"
```

## Integrations (now server-side)
Payments, SMS, and DUPR submission live **here** now, behind interfaces in
`internal/gateway` with mock implementations for dev. Real **Stripe**, **Twilio**,
and **DUPR partner API** clients implement the same interfaces and drop in via
config — this is the right home for the secrets + webhooks. Still to add next:
real auth/roles (replacing the local admin passcode) and cross-device live
updates (WebSocket/SSE; the Flutter client polls for now).

## Wiring the Flutter app to this API
Replace the app's local Drift `PickleRepository` with an HTTP client hitting
these endpoints (same method names map 1:1). The schema is identical, so it's a
transport swap, not a redesign.

## Production
Swap the SQLite DSN for Postgres (the migration is portable; `dupr_*`, money-in-
cents, UUID PKs, and `created/updated/synced_at` were all designed for it), put
it behind TLS, and add the real payment/SMS/DUPR integrations.
