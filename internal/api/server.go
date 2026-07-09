// Package api exposes the PlanMyPickle service over a small JSON REST API.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
)

type Server struct {
	svc *service.Service
	// phoneCheckin throttles the public phone check-in endpoint so it can't be
	// brute-forced to enumerate registrants.
	phoneCheckin *rateLimiter
	// regLimiter throttles the public self-registration endpoint per event so a
	// bot can't flood an event with fake players.
	regLimiter *rateLimiter
	// captcha verifies a Turnstile token on PUBLIC (anonymous) self-registration.
	// Active only when TURNSTILE_SECRET is set; otherwise it skips (fail-open).
	captcha *gateway.Captcha
}

// NewServer wires the routes and returns the HTTP handler.
func NewServer(svc *service.Service) http.Handler {
	s := &Server{
		svc:          svc,
		phoneCheckin: newRateLimiter(60, 60),
		regLimiter:   newRateLimiter(40, 60), // 40 self-registrations/min per event
		captcha:      gateway.NewTurnstile(os.Getenv("TURNSTILE_SECRET")),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// --- Public: spectator/shareable reads + the on-site self-service flows
	// reached from QR codes (register, pay, shirt order, self check-in). These
	// intentionally need no account so players and spectators have zero
	// friction. optionalAuth attaches the user when a token happens to be sent.
	// GET /events is the organizer DASHBOARD list — it returns only the caller's
	// own events (optionalAuth attaches the user; anonymous callers get nothing).
	// Spectator/registration flows use GET /events/{id} (single), not this list.
	mux.HandleFunc("GET /events", optionalAuth(s.listEvents))
	// Events the signed-in user is registered to PLAY in (the "Playing" home tab).
	mux.HandleFunc("GET /me/events", requireAuth(s.myEvents))
	mux.HandleFunc("GET /me/profile", requireAuth(s.myProfile))
	mux.HandleFunc("POST /me/profile", requireAuth(s.saveProfileDetails))
	mux.HandleFunc("GET /partners", requireAuth(s.partnerDirectory))
	mux.HandleFunc("POST /me/photo", requireAuth(s.uploadPhoto))
	mux.HandleFunc("DELETE /me/photo", requireAuth(s.clearPhoto))
	// DUPR account connection (SSO consent flow): the iframe URL, the callback
	// that stores the user's link, and the caller's connection status.
	mux.HandleFunc("GET /me/dupr/sso-url", requireAuth(s.duprSsoURL))
	mux.HandleFunc("POST /me/dupr/connect", requireAuth(s.duprConnect))
	mux.HandleFunc("GET /me/dupr/connection", requireAuth(s.duprConnection))
	mux.HandleFunc("DELETE /me/dupr/connection", requireAuth(s.duprDisconnect))
	// DUPR rating webhook (DUPR posts here — public; validated by clientId +
	// only updates an existing connected DUPR id).
	mux.HandleFunc("POST /dupr/webhook", s.duprWebhook)
	// In-app account deletion (Apple Guideline 5.1.1(v)): erases the caller's own
	// account + data. requireAuth scopes it to the authenticated user only.
	mux.HandleFunc("DELETE /me", requireAuth(s.deleteMe))
	// Aggregated activity feed across the user's events (powers the NewsFeed tab).
	mux.HandleFunc("GET /me/feed", requireAuth(s.myFeed))
	mux.HandleFunc("POST /me/posts", requireAuth(s.createPost))
	mux.HandleFunc("DELETE /me/posts/{id}", requireAuth(s.deletePost))
	// Organizer Stripe Connect (real payouts): start/resume onboarding + read
	// the connected-account status. Scoped to the authenticated organizer.
	mux.HandleFunc("POST /me/stripe/connect", requireAuth(s.stripeConnect))
	mux.HandleFunc("GET /me/stripe/status", requireAuth(s.stripeStatus))
	// Premium subscription (organizer pays PlanMyPickle): start Checkout, read
	// status, open the billing portal.
	mux.HandleFunc("POST /me/subscribe", requireAuth(s.subscribePremium))
	mux.HandleFunc("GET /me/subscription", requireAuth(s.subscriptionStatus))
	mux.HandleFunc("POST /me/billing-portal", requireAuth(s.billingPortal))
	mux.HandleFunc("GET /events/{id}", s.getEvent)
	mux.HandleFunc("GET /events/{id}/brackets", s.getBrackets)
	mux.HandleFunc("GET /events/{id}/standings", s.standings)
	mux.HandleFunc("GET /events/{id}/rounds", s.rounds)
	mux.HandleFunc("GET /events/{id}/matches", s.eventMatches)
	// The signed-in player's next match in this event (for the "your next" banner).
	mux.HandleFunc("GET /events/{id}/my-next-match", requireAuth(s.myNextMatch))
	mux.HandleFunc("GET /events/nearby", s.nearbyEvents)
	// Public marketing feed for planmypickle.com: recent/upcoming publicly-listed
	// events in a SAFE projection (no auth, no PII). Served cross-origin.
	mux.HandleFunc("GET /events/public", s.publicEvents)
	// Short-link redirect (SMS links): /r/<code> -> 302 to the full token URL.
	mux.HandleFunc("GET /r/{code}", s.shortLink)
	mux.HandleFunc("GET /events/{id}/busy-courts", s.busyCourts)
	mux.HandleFunc("GET /events/{id}/feed", optionalAuth(s.feedList))
	mux.HandleFunc("GET /events/{id}/roster", s.roster)
	// Public, PII-free player profile (rating + across-events box score).
	mux.HandleFunc("GET /players/{id}/profile", s.playerProfile)
	mux.HandleFunc("GET /feed/{id}/comments", optionalAuth(s.commentList))
	mux.HandleFunc("GET /brackets/{id}/matches", s.bracketMatches)
	mux.HandleFunc("GET /rounds/{id}/matches", s.roundMatches)
	mux.HandleFunc("GET /courts/nearby", s.nearbyCourts)
	mux.HandleFunc("GET /geocode", s.geocode)
	mux.HandleFunc("GET /city-autocomplete", requireAuth(s.cityAutocomplete))
	mux.HandleFunc("POST /events/{id}/register", optionalAuth(s.register))
	mux.HandleFunc("POST /events/{id}/import-roster", s.ownerOnly("event", "id", s.importRoster))
	mux.HandleFunc("POST /events/{id}/import-dupr", s.ownerOnly("event", "id", s.importDupr))

	// --- MLP-style team events (teams + rosters + ties) ---
	mux.HandleFunc("GET /events/{id}/teams", s.mlpListTeams)
	mux.HandleFunc("POST /events/{id}/teams", s.ownerOnly("event", "id", s.mlpCreateTeam))
	mux.HandleFunc("DELETE /events/{id}/teams/{teamId}", s.ownerOnly("event", "id", s.mlpRemoveTeam))
	mux.HandleFunc("POST /events/{id}/teams/{teamId}/rename", s.ownerOnly("event", "id", s.mlpRenameTeam))
	mux.HandleFunc("POST /events/{id}/teams/{teamId}/members", s.ownerOnly("event", "id", s.mlpAddTeamMember))
	mux.HandleFunc("DELETE /events/{id}/team-members/{memberId}", s.ownerOnly("event", "id", s.mlpRemoveTeamMember))
	mux.HandleFunc("POST /events/{id}/team-schedule", s.ownerOnly("event", "id", s.mlpGenerateTies))
	mux.HandleFunc("GET /events/{id}/ties", s.mlpListTies)
	mux.HandleFunc("POST /events/{id}/playoff", s.ownerOnly("event", "id", s.mlpGeneratePlayoff))
	mux.HandleFunc("GET /events/{id}/team-standings", s.mlpStandings)
	mux.HandleFunc("POST /events/{id}/lines/{matchId}/lineup", s.ownerOnly("event", "id", s.mlpSetLineup))
	mux.HandleFunc("POST /events/{id}/team-members/{memberId}/checkin", s.ownerOnly("event", "id", s.mlpCheckinMember))
	// One-time per-event Premium pass: owner-only Stripe Checkout.
	mux.HandleFunc("POST /events/{id}/premium-pass-checkout", s.ownerOnly("event", "id", s.startEventPassCheckout))
	// /pay and /shirt are public self-service (a registrant has no account), but
	// must prove ownership of the registration — the registration id is harvestable
	// from the public feed/roster, so without a check the endpoints are an IDOR
	// (mark anyone paid / overwrite anyone's shirt). optionalAuth attaches the event
	// owner's JWT when present; otherwise the caller must send the registration's
	// check_in_token (X-Registration-Token header or body). Gated by regLimiter too.
	mux.HandleFunc("POST /registrations/{id}/pay", optionalAuth(s.pay))
	mux.HandleFunc("POST /registrations/{id}/addons", optionalAuth(s.setAddons))
	mux.HandleFunc("POST /registrations/{id}/shirt", optionalAuth(s.saveShirt))
	// Start a Stripe Checkout Session for a registration's entry fee. Same IDOR
	// guard as /pay: the registrant proves ownership with the registration's
	// check_in_token (X-Registration-Token header or body), or the event owner's
	// JWT (optionalAuth). The id is harvestable from the public roster/feed, so a
	// proof is required before opening a hosted payment page in their name.
	mux.HandleFunc("POST /registrations/{id}/checkout", optionalAuth(s.checkout))
	mux.HandleFunc("POST /events/{id}/checkin", s.checkinByToken)
	mux.HandleFunc("POST /events/{id}/checkin-by-phone", s.checkinByPhone)
	mux.HandleFunc("POST /events/{id}/verify-admin", s.verifyAdmin)
	// Stripe webhook (server-to-server): NO auth wrapper — Stripe calls it and we
	// authenticate it by verifying the signature against STRIPE_WEBHOOK_SECRET.
	// The handler reads the RAW request body (signature is computed over the exact
	// bytes), so it must not pass through decode().
	mux.HandleFunc("POST /webhooks/stripe", s.stripeWebhook)

	// --- Authenticated: creating an event stamps the caller as its owner.
	mux.HandleFunc("POST /events", requireAuth(s.createEvent))
	// Clubs.
	mux.HandleFunc("POST /clubs", requireAuth(s.createClub))
	mux.HandleFunc("GET /me/clubs", requireAuth(s.myClubs))
	mux.HandleFunc("GET /clubs/{id}", optionalAuth(s.getClub))
	mux.HandleFunc("POST /clubs/{id}", requireAuth(s.updateClub))
	mux.HandleFunc("DELETE /clubs/{id}", requireAuth(s.deleteClub))
	mux.HandleFunc("POST /clubs/{id}/logo", requireAuth(s.uploadClubLogo))
	mux.HandleFunc("GET /clubs/{id}/members", s.clubMembers)
	mux.HandleFunc("GET /clubs/{id}/events", s.clubEvents)
	mux.HandleFunc("POST /clubs/{id}/join", requireAuth(s.joinClub))
	mux.HandleFunc("POST /clubs/{id}/leave", requireAuth(s.leaveClub))

	// Social graph: search players & follow them.
	mux.HandleFunc("GET /users/search", requireAuth(s.searchUsers))
	mux.HandleFunc("POST /users/{id}/follow", requireAuth(s.followUser))
	mux.HandleFunc("DELETE /users/{id}/follow", requireAuth(s.unfollowUser))
	mux.HandleFunc("GET /me/following", requireAuth(s.myFollowing))
	mux.HandleFunc("GET /me/followers", requireAuth(s.myFollowers))

	// --- Leagues (season / recurring play): owner-scoped WRITES, but READS are
	// open to participants too. Creating a league stamps the caller as its owner;
	// GET /leagues lists only the caller's OWNED leagues (organizer dashboard),
	// while GET /my-leagues returns owned ∪ participant (the player's view).
	mux.HandleFunc("POST /leagues", requireAuth(s.createLeague))
	mux.HandleFunc("GET /leagues", requireAuth(s.listLeagues))
	// The leagues the caller owns OR participates in (registered for a session, or
	// an entrant in a bracket) — the Play tab's "My leagues".
	mux.HandleFunc("GET /my-leagues", requireAuth(s.myLeagues))
	// READS: owner OR participant (leagueViewer). WRITES below stay owner-only.
	mux.HandleFunc("GET /leagues/{id}", s.leagueViewer("id", s.getLeague))
	mux.HandleFunc("POST /leagues/{id}/events", s.ownerOnly("league", "id", s.addEventToLeague))
	mux.HandleFunc("DELETE /leagues/{id}/events/{eventId}", s.ownerOnly("league", "id", s.removeEventFromLeague))
	mux.HandleFunc("GET /leagues/{id}/standings", s.leagueViewer("id", s.leagueStandings))
	// Set/clear the league banner (the client uploaded the image to Storage; this
	// just persists the public URL on the league row). Owner-only.
	mux.HandleFunc("POST /leagues/{id}/poster", s.ownerOnly("league", "id", s.setLeaguePoster))

	// --- Ladder League (organizer-driven): a division's (league_bracket) ladder
	// is an ordered ranking of entrants; recording a result applies the leapfrog
	// reorder. All writes are gated on the owning league via custom guards (the
	// resources are keyed on a division/entrant, not the league id, so ownerOnly
	// doesn't fit). Player self-service challenges are a FUTURE v2.
	// READS (ladder list/history) are open to participants via leagueBracketViewer;
	// WRITES stay owner-gated below.
	mux.HandleFunc("GET /league-brackets/{id}/ladder",
		s.leagueBracketViewer("id", s.listLadder))
	mux.HandleFunc("GET /league-brackets/{id}/ladder/history",
		s.leagueBracketViewer("id", s.ladderHistory))
	mux.HandleFunc("POST /league-brackets/{id}/ladder/entrants",
		s.ladderDivisionOwner("id", s.addLadderEntrant))
	mux.HandleFunc("POST /league-brackets/{id}/ladder/results",
		s.ladderDivisionOwner("id", s.recordLadderResult))
	mux.HandleFunc("DELETE /ladder-entrants/{id}",
		s.ladderEntrantOwner("id", s.removeLadderEntrant))

	// --- Team League (organizer-driven, SIMPLE single-fixture model): a
	// division's (league_bracket) teams + recorded fixtures. Standings (W-L +
	// win %) are computed from the fixtures on read, not stored. All writes are
	// gated on the owning league via custom guards (resources are keyed on a
	// division/team, not the league id, so ownerOnly doesn't fit).
	// READS (standings + fixtures) open to participants; WRITES stay owner-gated.
	mux.HandleFunc("GET /league-brackets/{id}/teams",
		s.leagueBracketViewer("id", s.listTeamStandings))
	mux.HandleFunc("GET /league-brackets/{id}/teams/fixtures",
		s.leagueBracketViewer("id", s.teamFixtures))
	mux.HandleFunc("POST /league-brackets/{id}/teams",
		s.teamDivisionOwner("id", s.addTeam))
	mux.HandleFunc("POST /league-brackets/{id}/teams/fixtures",
		s.teamDivisionOwner("id", s.recordFixture))
	mux.HandleFunc("DELETE /teams/{id}",
		s.teamOwnerOfTeam("id", s.removeTeam))

	// --- Flex League (organizer-driven, SELF-SCHEDULED round-robin): a division's
	// (league_bracket) fixed-partner teams (REUSING the `teams` table) + a
	// pre-generated round-robin SCHEDULE of matchups. Teams play on their own time;
	// the organizer records each matchup's result and standings (W-L + win %) are
	// computed from the COMPLETED matchups on read, not stored. All writes are
	// gated on the owning league via custom guards (resources are keyed on a
	// division/team/matchup, not the league id, so ownerOnly doesn't fit). Teams
	// are added/removed via the shared /teams routes above (Flex reuses that table).
	// READS (standings + schedule) open to participants; WRITES stay owner-gated.
	mux.HandleFunc("GET /league-brackets/{id}/flex/teams",
		s.leagueBracketViewer("id", s.listFlexStandings))
	mux.HandleFunc("GET /league-brackets/{id}/flex/matchups",
		s.leagueBracketViewer("id", s.listFlexMatchups))
	mux.HandleFunc("POST /league-brackets/{id}/flex/teams",
		s.flexDivisionOwner("id", s.addFlexTeam))
	mux.HandleFunc("POST /league-brackets/{id}/flex/generate",
		s.flexDivisionOwner("id", s.generateFlexSchedule))
	mux.HandleFunc("POST /league-brackets/{id}/flex/matchups/{matchupId}/result",
		s.flexDivisionOwner("id", s.recordFlexResult))
	mux.HandleFunc("DELETE /flex-teams/{id}",
		s.flexOwnerOfTeam("id", s.removeFlexTeam))

	// --- Owner-only: management actions require a valid token AND that the
	// caller owns the event behind the resource (see service.OwnerOf).
	mux.HandleFunc("POST /events/{id}", s.ownerOnly("event", "id", s.updateEvent))
	// Set/clear the event banner (the client uploaded the image to Storage; this
	// just persists the public URL on the event row). Kept separate from the
	// metadata edit so an edit never wipes the poster. Owner-only.
	mux.HandleFunc("POST /events/{id}/divisions", s.ownerOnly("event", "id", s.syncDivisions))
	mux.HandleFunc("POST /events/{id}/division-order", s.ownerOnly("event", "id", s.setDivisionOrder))
	mux.HandleFunc("POST /events/{id}/poster", s.ownerOnly("event", "id", s.setEventPoster))
	mux.HandleFunc("POST /events/{id}/sponsor-watermark", s.ownerOnly("event", "id", s.setSponsorWatermarkImage))
	mux.HandleFunc("POST /events/{id}/sponsor-watermark/settings", s.ownerOnly("event", "id", s.setSponsorWatermarkSettings))
	mux.HandleFunc("DELETE /events/{id}/sponsor-watermark", s.ownerOnly("event", "id", s.clearSponsorWatermark))
	mux.HandleFunc("POST /events/{id}/scoreboard-theme", s.ownerOnly("event", "id", s.setScoreboardTheme))
	mux.HandleFunc("DELETE /events/{id}", s.ownerOnly("event", "id", s.deleteEvent))
	mux.HandleFunc("GET /events/{id}/registrations", s.ownerOnly("event", "id", s.registrations))
	mux.HandleFunc("GET /events/{id}/finance", s.ownerOnly("event", "id", s.financeEntries))
	// Downloadable results export (standings + matches) for the organizer.
	mux.HandleFunc("GET /events/{id}/results.csv", s.ownerOnly("event", "id", s.resultsCSV))
	mux.HandleFunc("GET /events/{id}/roster.csv", s.ownerOnly("event", "id", s.rosterCSV))
	// League "season roster": copy a previous session's roster into this event.
	mux.HandleFunc("POST /events/{id}/copy-roster", s.ownerOnly("event", "id", s.copyRoster))
	// Take a court offline / swap its unplayed games onto another court.
	mux.HandleFunc("POST /events/{id}/remap-court", s.ownerOnly("event", "id", s.remapCourt))
	mux.HandleFunc("GET /events/{id}/sanction.csv", s.ownerOnly("event", "id", s.sanctionCSV))

	// Vendor Village: public list (spectators see APPROVED booths; the owner
	// also sees pending applications); organizer-only create/update/delete +
	// the "push this deal to players" send. The public "Become a vendor" form
	// posts applications (rate-limited + captcha, like self-registration);
	// approve/reject is owner-only.
	mux.HandleFunc("GET /events/{id}/vendors", optionalAuth(s.listVendors))
	mux.HandleFunc("POST /events/{id}/vendors", s.ownerOnly("event", "id", s.createVendor))
	mux.HandleFunc("POST /events/{id}/vendor-apply", s.applyVendor)
	mux.HandleFunc("POST /vendors/{id}", s.ownerOnly("vendor", "id", s.updateVendor))
	mux.HandleFunc("DELETE /vendors/{id}", s.ownerOnly("vendor", "id", s.deleteVendor))
	mux.HandleFunc("POST /vendors/{id}/notify", s.ownerOnly("vendor", "id", s.notifyVendorDeal))
	mux.HandleFunc("POST /vendors/{id}/status", s.ownerOnly("vendor", "id", s.setVendorStatus))
	// Booth-fee self-service (vendors have no accounts — pay_token gates, like
	// registrations' check_in_token): the pay page's info read + Stripe checkout.
	mux.HandleFunc("GET /vendors/{id}/pay-info", optionalAuth(s.vendorPayInfo))
	mux.HandleFunc("POST /vendors/{id}/checkout", optionalAuth(s.vendorCheckout))
	// Organizer confirms an off-platform booth payment (cash / Zelle).
	mux.HandleFunc("POST /vendors/{id}/mark-paid", s.ownerOnly("vendor", "id", s.vendorMarkPaid))
	// Tap-through counter (public, best-effort) + court sponsors for the board.
	mux.HandleFunc("POST /vendors/{id}/click", s.vendorClick)
	mux.HandleFunc("GET /events/{id}/court-sponsors", s.courtSponsors)
	// Team banner upload (MLP events) — owner-only via the team's event.
	mux.HandleFunc("POST /teams/{id}/banner", s.ownerOnly("team", "id", s.uploadTeamBanner))

	// "Player Score Confirm" (Premium add-on): participants act via their own
	// registration check_in_token (?t=) — winner reports, loser confirms or
	// disputes; the organizer's score list powers match-card chips.
	mux.HandleFunc("GET /matches/{id}/score-report", optionalAuth(s.scoreReportState))
	mux.HandleFunc("POST /matches/{id}/score-report", optionalAuth(s.scoreReport))
	mux.HandleFunc("POST /matches/{id}/score-report/confirm", optionalAuth(s.scoreConfirm))
	mux.HandleFunc("POST /matches/{id}/score-report/dispute", optionalAuth(s.scoreDispute))
	mux.HandleFunc("GET /events/{id}/score-reports", s.ownerOnly("event", "id", s.listScoreReports))
	mux.HandleFunc("POST /events/{id}/finance", s.ownerOnly("event", "id", s.addFinanceEntry))
	mux.HandleFunc("DELETE /finance/{id}", s.ownerOnly("finance", "id", s.deleteFinanceEntry))
	mux.HandleFunc("GET /events/{id}/checklist", s.ownerOnly("event", "id", s.checklist))
	mux.HandleFunc("POST /events/{id}/checklist", s.ownerOnly("event", "id", s.addChecklistItem))
	mux.HandleFunc("POST /checklist/{id}/check", s.ownerOnly("checklist", "id", s.setChecklistChecked))
	mux.HandleFunc("DELETE /checklist/{id}", s.ownerOnly("checklist", "id", s.deleteChecklistItem))
	mux.HandleFunc("GET /events/{id}/freebies", s.ownerOnly("event", "id", s.freebies))
	mux.HandleFunc("POST /events/{id}/freebies", s.ownerOnly("event", "id", s.addFreebie))
	mux.HandleFunc("POST /freebies/{id}", s.ownerOnly("freebie", "id", s.updateFreebie))
	mux.HandleFunc("POST /freebies/{id}/adjust", s.ownerOnly("freebie", "id", s.adjustFreebie))
	mux.HandleFunc("DELETE /freebies/{id}", s.ownerOnly("freebie", "id", s.deleteFreebie))
	mux.HandleFunc("POST /events/{id}/schedule", s.ownerOnly("event", "id", s.schedule))
	mux.HandleFunc("POST /events/{id}/manual-game", s.ownerOnly("event", "id", s.manualGame))
	mux.HandleFunc("POST /events/{id}/clear-arrangement", s.ownerOnly("event", "id", s.clearArrangement))
	mux.HandleFunc("POST /events/{id}/auto-schedule", s.ownerOnly("event", "id", s.autoSchedule))
	mux.HandleFunc("POST /events/{id}/game-duration", s.ownerOnly("event", "id", s.setGameDuration))
	mux.HandleFunc("POST /events/{id}/start-time", s.ownerOnly("event", "id", s.setStartTime))
	mux.HandleFunc("POST /events/{id}/fill-demo-players", s.ownerOnly("event", "id", s.fillRandomPlayers))
	// Demo helper: enroll the fixed DUPR UAT test accounts into a sanctioned event.
	mux.HandleFunc("POST /events/{id}/register-dupr-testers", s.ownerOnly("event", "id", s.registerDuprTesters))
	// Reverse an event's submitted results on DUPR (the delete leg of the round-trip).
	mux.HandleFunc("POST /events/{id}/dupr/remove", s.ownerOnly("event", "id", s.removeFromDupr))
	mux.HandleFunc("POST /events/{id}/feed", s.ownerOnly("event", "id", s.feedPost))
	mux.HandleFunc("DELETE /feed/{id}", s.ownerOnly("feed_item", "id", s.feedDelete))
	// Feed social — any signed-in user may react/comment (not just the owner).
	mux.HandleFunc("POST /feed/{id}/react", requireAuth(s.feedReact))
	mux.HandleFunc("POST /feed/{id}/comments", requireAuth(s.commentAdd))
	mux.HandleFunc("DELETE /comments/{id}", requireAuth(s.commentDelete))
	mux.HandleFunc("POST /events/{id}/dupr/import", s.ownerOnly("event", "id", s.duprImport))
	// Scorekeeper auth: the event owner (JWT) OR a volunteer holding the event's
	// admin passcode (X-Event-Passcode) may record a match score.
	mux.HandleFunc("POST /matches/{id}/score", s.ownerOrPasscode("match", "id", s.recordScore))
	mux.HandleFunc("POST /matches/{id}/forfeit", s.ownerOnly("match", "id", s.forfeitMatch))
	mux.HandleFunc("POST /matches/{id}/start", s.ownerOnly("match", "id", s.startMatch))
	mux.HandleFunc("POST /matches/{id}/unstart", s.ownerOnly("match", "id", s.unstartMatch))
	mux.HandleFunc("POST /matches/{id}/swap", s.ownerOnly("match", "id", s.swapMatchPlayer))
	// Cross-match swap spans two matches, so ownerOnly (one path id) won't fit:
	// requireAuth + verify the caller owns both matches' events in the handler.
	mux.HandleFunc("POST /matches/swap-cross", requireAuth(s.swapMatchPlayersCross))
	mux.HandleFunc("POST /matches/{id}/court", s.ownerOnly("match", "id", s.setMatchCourt))
	mux.HandleFunc("POST /matches/{id}/duration", s.ownerOnly("match", "id", s.setMatchDuration))
	mux.HandleFunc("POST /matches/{id}/day", s.ownerOnly("match", "id", s.setMatchDay))
	mux.HandleFunc("DELETE /matches/{id}", s.ownerOnly("match", "id", s.deleteMatch))
	mux.HandleFunc("POST /matches/{id}/dupr/remove", s.ownerOnly("match", "id", s.removeMatchFromDupr))
	mux.HandleFunc("POST /events/{id}/breaks", s.ownerOnly("event", "id", s.setEventBreaks))
	mux.HandleFunc("POST /events/{id}/day-cap", s.ownerOnly("event", "id", s.setDayCap))
	mux.HandleFunc("POST /events/{id}/day-ends", s.ownerOnly("event", "id", s.setDayEnds))
	mux.HandleFunc("GET /brackets/{id}/playoff-seed", s.ownerOnly("bracket", "id", s.playoffSeed))
	mux.HandleFunc("POST /brackets/{id}/playoff", s.ownerOnly("bracket", "id", s.playoff))
	mux.HandleFunc("POST /rounds/{id}/start", s.ownerOnly("round", "id", s.startRound))
	mux.HandleFunc("POST /registrations/{id}/checkin", s.ownerOnly("registration", "id", s.checkin))
	mux.HandleFunc("POST /registrations/{id}/uncheckin", s.ownerOnly("registration", "id", s.uncheckin))
	mux.HandleFunc("POST /registrations/{id}/mark-paid", s.ownerOnly("registration", "id", s.markPaid))
	mux.HandleFunc("POST /registrations/{id}/details", s.ownerOnly("registration", "id", s.updateRegistrationDetails))
	mux.HandleFunc("POST /registrations/{id}/partner", s.ownerOnly("registration", "id", s.setPartner))
	mux.HandleFunc("DELETE /registrations/{id}", s.ownerOnly("registration", "id", s.deleteRegistration))
	mux.HandleFunc("DELETE /rounds/{id}", s.ownerOnly("round", "id", s.deleteRound))
	mux.HandleFunc("GET /events/{id}/dupr-status", s.ownerOnly("event", "id", s.duprStatuses))

	// --- Demo seeding: load a sample tournament owned by the signed-in user, so
	// the "Load demo" buttons produce events the caller can actually manage.
	// requireAuth keeps it from being an anonymous data-injection endpoint.
	mux.HandleFunc("POST /dev/seed", requireAuth(s.seedDemo))
	mux.HandleFunc("POST /dev/seed-playoff", requireAuth(s.seedPlayoffDemo))
	// QA-only: seed a 30/150/80-player TEST tournament (Profile-tab buttons).
	mux.HandleFunc("POST /dev/seed-test", requireAuth(s.seedTestTournament))
	// Seed a set of PUBLIC tournaments near San Diego so the Play/Nearby tab
	// (and its map) has content. Owned by the caller — deletable afterward.
	mux.HandleFunc("POST /dev/seed-nearby", requireAuth(s.seedNearbyDemo))
	mux.HandleFunc("POST /dev/unseed-nearby", requireAuth(s.unseedNearbyDemo))
	// Rename leftover "TEST ·" events + "Test Courts" venue to legit names.
	mux.HandleFunc("POST /dev/tidy-tests", requireAuth(s.tidyTestEvents))

	return withCORS(mux)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.svc.ListEvents(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// myEvents returns the events the signed-in user is registered to play in.
func (s *Server) myEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.svc.MyEvents(userID(r), userEmail(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) myNextMatch(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.MyNextMatch(r.PathValue("id"), userID(r), userEmail(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"match": m})
}

// myProfile returns the signed-in user's saved player details to pre-fill the
// registration form.
func (s *Server) myProfile(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.MyProfile(userID(r), userEmail(r)))
}

// uploadPhoto stores the caller's avatar image (raw JPEG/PNG body) and returns
// its public URL. Body is hard-capped at 6 MB; the service validates type/size.
func (s *Server) uploadPhoto(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 6<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	url, err := s.svc.SetMyPhoto(userID(r), r.Header.Get("Content-Type"), data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"photoUrl": url})
}

// clearPhoto removes the caller's uploaded avatar (fall back to mascot/initials).
func (s *Server) clearPhoto(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ClearMyPhoto(userID(r)); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// myFeed returns the caller's cross-event activity stream (the NewsFeed tab).
// saveProfileDetails stores the caller's partner-finder fields (gender/city/
// seeking flag).
func (s *Server) saveProfileDetails(w http.ResponseWriter, r *http.Request) {
	var req model.ProfileDetailsRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetMyProfileDetails(userID(r), req.Gender, req.City, req.SeekingPartner); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// partnerDirectory lists "looking for a partner" players (?gender=&city=&min=&max=).
func (s *Server) partnerDirectory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parseF := func(k string) *float64 {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return &f
			}
		}
		return nil
	}
	res, err := s.svc.PartnerDirectory(userID(r), q.Get("gender"), q.Get("city"),
		parseF("min"), parseF("max"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) myFeed(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.MyFeed(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// createPost creates a standalone community (user) post from the NewsFeed composer.
func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	fi, err := s.svc.CreateCommunityPost(userID(r), userEmail(r), req.Text)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fi)
}

// deletePost removes the caller's own community post.
func (s *Server) deletePost(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteCommunityPost(r.PathValue("id"), userID(r)); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	var req model.CreateEventRequest
	if !decode(w, r, &req) {
		return
	}
	// Organizing is FREE — anyone can create + run a tournament (the engine is
	// never paywalled). Premium gates only specific features: CreateEvent itself
	// returns ErrPremiumRequired for a DUPR-sanctioned event, and advanced draws /
	// remove-branding / clubs are gated elsewhere.
	id, err := s.svc.CreateEvent(req, userID(r))
	if err != nil {
		if errors.Is(err, service.ErrPremiumRequired) {
			writeErr(w, http.StatusPaymentRequired, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// createLeague creates a league owned by the authenticated caller.
func (s *Server) createLeague(w http.ResponseWriter, r *http.Request) {
	var req model.CreateLeagueRequest
	if !decode(w, r, &req) {
		return
	}
	// Organizing (incl. leagues) is FREE; the Club tier monetizes the durable
	// system-of-record (cross-season standings + roster), not league creation.
	id, err := s.svc.CreateLeague(userID(r), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// listLeagues returns the leagues owned by the authenticated caller.
func (s *Server) listLeagues(w http.ResponseWriter, r *http.Request) {
	leagues, err := s.svc.ListLeagues(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, leagues)
}

// myLeagues returns the leagues the authenticated caller owns OR participates in
// (registered for a session, or an entrant in a bracket) — the Play tab's
// "My leagues".
func (s *Server) myLeagues(w http.ResponseWriter, r *http.Request) {
	leagues, err := s.svc.MyLeagues(userID(r), userEmail(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, leagues)
}

// getLeague returns a league plus its sessions (events), ordered by start date.
func (s *Server) getLeague(w http.ResponseWriter, r *http.Request) {
	detail, err := s.svc.GetLeague(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// addEventToLeague links an existing event into the league. ownerOnly("league")
// proves the caller owns the league; here we ALSO verify the caller owns the
// event being added, so an organizer can't pull a stranger's event into their league.
func (s *Server) addEventToLeague(w http.ResponseWriter, r *http.Request) {
	var req model.AddEventToLeagueRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.EventID) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("eventId is required"))
		return
	}
	owner, err := s.svc.OwnerOf("event", req.EventID)
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if owner == "" || owner != userID(r) {
		writeErr(w, http.StatusForbidden, errForbidden)
		return
	}
	if err := s.svc.AddEventToLeague(r.PathValue("id"), req.EventID); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

// removeEventFromLeague unlinks an event from the league (owner-only via the
// league path id; the event must currently belong to this league).
func (s *Server) removeEventFromLeague(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RemoveEventFromLeague(r.PathValue("id"), r.PathValue("eventId")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// leagueStandings returns the cumulative standings across all the league's
// events' completed matches (owner-only).
func (s *Server) leagueStandings(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.LeagueStandings(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// listLadder returns a division's ladder, ordered by position (1 = top). This is
// also the standings — the ladder order IS the ranking. Owner-gated.
func (s *Server) listLadder(w http.ResponseWriter, r *http.Request) {
	entrants, err := s.svc.ListLadder(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entrants)
}

// addLadderEntrant appends an entrant to the BOTTOM of a division's ladder.
func (s *Server) addLadderEntrant(w http.ResponseWriter, r *http.Request) {
	var req model.AddLadderEntrantRequest
	if !decode(w, r, &req) {
		return
	}
	entrant, err := s.svc.AddLadderEntrant(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entrant)
}

// recordLadderResult records a match between two entrants and applies the
// leapfrog reorder atomically. Returns the recorded match.
func (s *Server) recordLadderResult(w http.ResponseWriter, r *http.Request) {
	var req model.RecordLadderResultRequest
	if !decode(w, r, &req) {
		return
	}
	m, err := s.svc.RecordLadderResult(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ladderHistory returns a division's recorded matches, newest first.
func (s *Server) ladderHistory(w http.ResponseWriter, r *http.Request) {
	matches, err := s.svc.LadderHistory(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, matches)
}

// removeLadderEntrant deletes an entrant and closes the ladder gap below it.
func (s *Server) removeLadderEntrant(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RemoveLadderEntrant(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// listTeamStandings returns a division's teams with their computed W-L record
// and win %, ordered by wins (then win %). This is the standings view (computed
// from the fixtures, not stored). Owner-gated.
func (s *Server) listTeamStandings(w http.ResponseWriter, r *http.Request) {
	standings, err := s.svc.ListTeamStandings(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, standings)
}

// addTeam registers a team on a division.
func (s *Server) addTeam(w http.ResponseWriter, r *http.Request) {
	var req model.AddTeamRequest
	if !decode(w, r, &req) {
		return
	}
	team, err := s.svc.AddTeam(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, team)
}

// recordFixture records a single fixture result between two teams. Returns the
// recorded fixture; standings recompute on the next read.
func (s *Server) recordFixture(w http.ResponseWriter, r *http.Request) {
	var req model.RecordFixtureRequest
	if !decode(w, r, &req) {
		return
	}
	f, err := s.svc.RecordFixture(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// teamFixtures returns a division's recorded fixtures, newest first.
func (s *Server) teamFixtures(w http.ResponseWriter, r *http.Request) {
	fixtures, err := s.svc.TeamFixtures(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, fixtures)
}

// removeTeam deletes a team; its fixture history cascade-deletes.
func (s *Server) removeTeam(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RemoveTeam(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// listFlexStandings returns a Flex division's teams with their computed W-L
// record and win %, ordered by wins (then win %) — computed from the COMPLETED
// matchups, not stored. Owner-gated.
func (s *Server) listFlexStandings(w http.ResponseWriter, r *http.Request) {
	standings, err := s.svc.ListFlexStandings(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, standings)
}

// listFlexMatchups returns a Flex division's full generated schedule (pending +
// completed matchups). Owner-gated.
func (s *Server) listFlexMatchups(w http.ResponseWriter, r *http.Request) {
	matchups, err := s.svc.ListFlexMatchups(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, matchups)
}

// addFlexTeam registers a team on a Flex division (reuses the `teams` table).
func (s *Server) addFlexTeam(w http.ResponseWriter, r *http.Request) {
	var req model.AddTeamRequest
	if !decode(w, r, &req) {
		return
	}
	team, err := s.svc.AddFlexTeam(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, team)
}

// generateFlexSchedule creates the division's round-robin schedule (every team
// pair → a pending matchup), idempotently. Returns the count created.
func (s *Server) generateFlexSchedule(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.GenerateFlexSchedule(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"matchups": n})
}

// recordFlexResult records the result of a pending matchup (score + winner),
// flipping it to completed. The path carries both the division id and the
// matchup id; the service binds the matchup to that division before writing.
func (s *Server) recordFlexResult(w http.ResponseWriter, r *http.Request) {
	var req model.RecordFlexResultRequest
	if !decode(w, r, &req) {
		return
	}
	m, err := s.svc.RecordFlexResult(r.PathValue("id"), r.PathValue("matchupId"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// removeFlexTeam deletes a Flex team; its matchups cascade-delete and standings
// recompute on the next read.
func (s *Server) removeFlexTeam(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RemoveFlexTeam(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// updateEvent edits an existing event's metadata (owner-only). Structural
// format / brackets are intentionally not editable here.
func (s *Server) updateEvent(w http.ResponseWriter, r *http.Request) {
	var req model.CreateEventRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.UpdateEvent(r.PathValue("id"), req); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// syncDivisions reconciles an event's divisions with the provided list (edit
// flow): updates existing (by id), inserts new (no id), deletes removed empties.
// Returns the names of divisions that COULDN'T be deleted (they still have
// players or matches) so the client can explain.
// duprPlayer looks up a player's live DUPR ratings by DUPR id — a smoke test for
// the partner integration (mock data until the DUPR_* env vars are configured).
// duprWebhook receives DUPR RATING / RATING_SEED events and refreshes the
// matching connected user's cached rating. Public (DUPR posts here), so it is
// FAIL-CLOSED on a shared secret: DUPR_WEBHOOK_SECRET must be set and arrive
// either in the X-Webhook-Token header (preferred — keeps it out of URL/proxy
// logs) or the ?token= query param (back-compat with an already-registered
// webhook URL). Compared in constant time to avoid a timing side-channel.
// Always 200 quickly so DUPR doesn't retry-storm.
func (s *Server) duprWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("DUPR_WEBHOOK_SECRET")
	token := r.Header.Get("X-Webhook-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if secret == "" ||
		subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
		// Unconfigured or wrong token → reject, so no one can rewrite ratings.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var p struct {
		ClientID string `json:"clientId"`
		Event    string `json:"event"`
		Message  struct {
			DuprID string `json:"duprId"`
			Rating struct {
				Singles json.RawMessage `json:"singles"`
				Doubles json.RawMessage `json:"doubles"`
			} `json:"rating"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		// Silently ignoring a malformed body is an operational blind spot — a DUPR
		// schema change would stop all rating updates with zero logs.
		shown := body
		if len(shown) > 200 {
			shown = shown[:200]
		}
		log.Printf("dupr webhook: unparseable payload: %v: %s", err, shown)
		w.WriteHeader(http.StatusOK)
		return
	}
	// Defense-in-depth signal: warn if the payload's clientId doesn't match our
	// configured client key. We only LOG (don't reject) because DUPR's clientId
	// and our client KEY may be different values — rejecting on an unconfirmed
	// field would fail-close every legit rating webhook. The shared secret is the
	// real gate. If DUPR confirms clientId == clientKey, this can become a reject.
	if key := os.Getenv("DUPR_CLIENT_KEY"); key != "" && p.ClientID != "" && p.ClientID != key {
		log.Printf("dupr webhook: clientId %q != configured client key (allowed; secret already verified)", p.ClientID)
	}
	if p.Message.DuprID != "" {
		if err := s.svc.ApplyDuprRating(p.Message.DuprID,
			ratingPtr(p.Message.Rating.Doubles),
			ratingPtr(p.Message.Rating.Singles)); err != nil {
			log.Printf("dupr webhook: apply rating for %s failed: %v",
				p.Message.DuprID, err)
		}
	} else {
		log.Printf("dupr webhook: no duprId in payload (event=%q) — ignored", p.Event)
	}
	w.WriteHeader(http.StatusOK)
}

// ratingPtr parses a DUPR rating value (number, "NR", "", or null) → *float64.
func ratingPtr(raw json.RawMessage) *float64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" || strings.EqualFold(s, "NR") {
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return &f
	}
	return nil
}

// duprSsoURL returns the iframe URL (+ origin) for the DUPR account-connect flow.
func (s *Server) duprSsoURL(w http.ResponseWriter, r *http.Request) {
	url, origin := s.svc.DuprSsoURL()
	writeJSON(w, http.StatusOK, map[string]any{"url": url, "origin": origin})
}

// duprConnect stores the caller's DUPR link captured by the SSO iframe, then
// returns the resulting (token-free) connection for display.
func (s *Server) duprConnect(w http.ResponseWriter, r *http.Request) {
	var in model.DuprConnectInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.svc.ConnectDupr(userID(r), in); err != nil {
		if errors.Is(err, service.ErrDuprIDTaken) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	conn, err := s.svc.DuprConnection(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

// duprConnection returns the caller's DUPR connection status (token-free).
func (s *Server) duprConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := s.svc.DuprConnection(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

// duprDisconnect fully unlinks the caller's own DUPR account (self-only).
func (s *Server) duprDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DisconnectDupr(userID(r)); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"connected": false})
}

// setDivisionOrder reorders the event's divisions so the organizer controls
// which one the scheduler lays down first. Body: {"order": ["bracketId", ...]}.
func (s *Server) setDivisionOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Order []string `json:"order"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetDivisionOrder(r.PathValue("id"), req.Order); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncDivisions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Divisions []model.BracketInput `json:"divisions"`
	}
	if !decode(w, r, &req) {
		return
	}
	blocked, err := s.svc.SyncDivisions(r.PathValue("id"), req.Divisions)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if blocked == nil {
		blocked = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": blocked})
}

// setEventPoster sets (or clears, when posterUrl is empty) the event's banner
// URL — the public Storage URL the client uploaded the image to. Owner-only;
// kept separate from updateEvent so a metadata edit never touches the poster.
func (s *Server) setEventPoster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PosterURL string `json:"posterUrl"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetEventPoster(r.PathValue("id"), req.PosterURL); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "set"})
}

// setSponsorWatermarkImage uploads the event's sponsor watermark image (owner-only
// via the path id). JPEG/PNG up to ~5 MB; returns the stored URL.
func (s *Server) setSponsorWatermarkImage(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 6<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	url, err := s.svc.SetSponsorWatermarkImage(
		r.PathValue("id"), r.Header.Get("Content-Type"), data)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// setSponsorWatermarkSettings saves the watermark placement (opacity/scale/position).
func (s *Server) setSponsorWatermarkSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string  `json:"url"`
		Opacity  float64 `json:"opacity"`
		Scale    float64 `json:"scale"`
		Position string  `json:"position"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetSponsorWatermarkSettings(
		r.PathValue("id"), req.URL, req.Opacity, req.Scale, req.Position); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// clearSponsorWatermark removes the event's watermark image.
func (s *Server) clearSponsorWatermark(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ClearSponsorWatermark(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// setScoreboardTheme saves the per-event live-board look (colors + font).
func (s *Server) setScoreboardTheme(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Theme map[string]any `json:"theme"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetScoreboardTheme(r.PathValue("id"), req.Theme); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// setLeaguePoster sets (or clears, when posterUrl is empty) the league's banner
// URL. Owner-only (via the league path id).
func (s *Server) setLeaguePoster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PosterURL string `json:"posterUrl"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetLeaguePoster(r.PathValue("id"), req.PosterURL); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "set"})
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.GetEvent(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteEvent(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// deleteMe erases the authenticated caller's own account + data (no path param —
// the user id comes from the verified token, so a user can only delete themselves).
func (s *Server) deleteMe(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteAccount(userID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) seedDemo(w http.ResponseWriter, r *http.Request) {
	id, err := s.svc.SeedDemo(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"eventId": id})
}

// seedTestTournament creates a 30/150/80-player TEST tournament (the Profile-tab
// dev buttons). Gated to a small allow-list of QA accounts.
func (s *Server) seedTestTournament(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(userEmail(r)))
	if email != "rolando.naranjo0420@gmail.com" && email != "krizhia_roxas29@yahoo.com" {
		writeErr(w, http.StatusForbidden, errors.New("not allowed"))
		return
	}
	id, err := s.svc.SeedTestTournament(userID(r), r.URL.Query().Get("kind"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"eventId": id})
}

func (s *Server) seedNearbyDemo(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(userEmail(r)))
	if email != "rolando.naranjo0420@gmail.com" && email != "krizhia_roxas29@yahoo.com" {
		writeErr(w, http.StatusForbidden, errors.New("not allowed"))
		return
	}
	n, err := s.svc.SeedNearbyEvents(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int{"seeded": n})
}

func (s *Server) tidyTestEvents(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(userEmail(r)))
	if email != "rolando.naranjo0420@gmail.com" && email != "krizhia_roxas29@yahoo.com" {
		writeErr(w, http.StatusForbidden, errors.New("not allowed"))
		return
	}
	n, err := s.svc.TidyTestEvents(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"renamed": n})
}

func (s *Server) unseedNearbyDemo(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(userEmail(r)))
	if email != "rolando.naranjo0420@gmail.com" && email != "krizhia_roxas29@yahoo.com" {
		writeErr(w, http.StatusForbidden, errors.New("not allowed"))
		return
	}
	if err := s.svc.RemoveNearbyEvents(userID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) seedPlayoffDemo(w http.ResponseWriter, r *http.Request) {
	id, err := s.svc.SeedPlayoffDemo(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"eventId": id})
}

func (s *Server) getBrackets(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.GetBrackets(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req model.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	// Per-event throttle so the public self-registration link can't be flooded
	// with fake players. Generous enough for a real registration rush.
	if !s.regLimiter.allow(r.PathValue("id")) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many registrations right now, try again shortly"))
		return
	}
	// Bot check on the PUBLIC form only: anonymous (no token) self-registration
	// must pass Turnstile. An authenticated organizer adding a player is trusted
	// and skips it. No-op unless TURNSTILE_SECRET is configured.
	if userID(r) == "" && !s.captcha.Verify(req.CaptchaToken, "") {
		writeErr(w, http.StatusForbidden, errors.New("please complete the human check and try again"))
		return
	}
	// Link the player to the caller's account ONLY when a logged-in user is
	// registering themselves (req.Self). An organizer adding others does not.
	linkUserID := ""
	if req.Self {
		linkUserID = userID(r)
	}
	reg, err := s.svc.RegisterPlayer(r.PathValue("id"), req, linkUserID)
	if err != nil {
		// A duplicate registration is a 409 so the client can show a friendly
		// "already registered" message rather than a generic error.
		if errors.Is(err, service.ErrAlreadyRegistered) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		// Sanctioned event requires a connected DUPR account to self-register.
		if errors.Is(err, service.ErrDuprNotConnected) {
			writeErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		// Premium / Verified event requires a DUPR entitlement the player lacks.
		if errors.Is(err, service.ErrDuprEntitlementRequired) {
			writeErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if name := strings.TrimSpace(req.FullName); name != "" {
		s.svc.AddFeedItem(r.PathValue("id"), "registered", name+" registered", reg.ID)
	}
	// Branded confirmation email, off the request path (best-effort; a mail
	// hiccup never fails the registration). Fires only from this handler —
	// bulk imports and seeders deliberately don't email.
	if email := strings.TrimSpace(req.Email); email != "" {
		bracketID := ""
		if reg.BracketID != nil {
			bracketID = *reg.BracketID
		}
		go s.svc.SendRegistrationEmail(
			r.PathValue("id"), email, req.FullName, bracketID)
	}
	// For anonymous self-registration, tell the client whether an account already
	// exists for this email, so the thank-you screen nudges sign-in vs sign-up.
	if userID(r) == "" {
		if email := strings.TrimSpace(req.Email); email != "" {
			exists := s.svc.AccountExists(email)
			reg.AccountExists = &exists
		}
	}
	writeJSON(w, http.StatusCreated, reg)
}

func (s *Server) financeEntries(w http.ResponseWriter, r *http.Request) {
	entries, err := s.svc.FinanceEntries(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// resultsCSV streams the event's results export as a CSV download (owner-only).
func (s *Server) resultsCSV(w http.ResponseWriter, r *http.Request) {
	data, err := s.svc.ResultsCSV(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="results.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// sanctionCSV streams the sanction-ready export (players + DUPR ids + per-game
// scores + DUPR submission state per completed match). Owner-only.
func (s *Server) sanctionCSV(w http.ResponseWriter, r *http.Request) {
	data, err := s.svc.SanctionCSV(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="sanction.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// listVendors returns an event's Vendor Village entries. Anonymous/spectator
// callers get approved vendors in a PII-free shape; the event owner also gets
// pending applications with the applicant's contact details.
func (s *Server) listVendors(w http.ResponseWriter, r *http.Request) {
	eventID := r.PathValue("id")
	includeAll := false
	if uid := userID(r); uid != "" {
		if owner, err := s.svc.OwnerOf("event", eventID); err == nil && owner == uid {
			includeAll = true
		}
	}
	vs, err := s.svc.ListVendors(eventID, includeAll)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, vs)
}

// applyVendor records a PUBLIC vendor application from the "Become a vendor"
// link. Same abuse guards as self-registration: per-event rate limit + captcha
// for anonymous callers.
func (s *Server) applyVendor(w http.ResponseWriter, r *http.Request) {
	var req model.VendorApplyRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.regLimiter.allow("vendor:" + r.PathValue("id")) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many applications right now, try again shortly"))
		return
	}
	if userID(r) == "" && !s.captcha.Verify(req.CaptchaToken, "") {
		writeErr(w, http.StatusForbidden, errors.New("please complete the human check and try again"))
		return
	}
	v, err := s.svc.ApplyVendor(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// setVendorStatus approves/rejects a vendor application (owner-only). An
// approval emails the applicant (best-effort) — with the booth-fee pay link
// when a fee is set.
func (s *Server) setVendorStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := s.svc.SetVendorStatus(r.PathValue("id"), req.Status)
	if err != nil {
		status(w, err)
		return
	}
	if req.Status == "approved" {
		go s.svc.SendVendorApprovedEmail(v)
	}
	writeJSON(w, http.StatusOK, v)
}

// authorizeVendor gates the public booth-fee endpoints against IDOR: per-vendor
// rate limit, then the vendor's pay_token (?t= / X-Vendor-Token) or the event
// owner's JWT.
func (s *Server) authorizeVendor(w http.ResponseWriter, r *http.Request) bool {
	vendorID := r.PathValue("id")
	if !s.regLimiter.allow("vendor-action:" + vendorID) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many requests right now, try again shortly"))
		return false
	}
	token := strings.TrimSpace(r.URL.Query().Get("t"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Vendor-Token"))
	}
	allowed, err := s.svc.AuthorizeVendorAction(vendorID, token, userID(r))
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, errors.New("not allowed"))
		return false
	}
	return true
}

// vendorPayInfo returns what the booth-fee pay page shows (token-gated).
func (s *Server) vendorPayInfo(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeVendor(w, r) {
		return
	}
	info, err := s.svc.GetVendorPayInfo(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// vendorCheckout opens a Stripe Checkout for the booth fee (token-gated) and
// returns its URL.
func (s *Server) vendorCheckout(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeVendor(w, r) {
		return
	}
	var req struct {
		SuccessURL string `json:"successUrl"`
		CancelURL  string `json:"cancelUrl"`
	}
	if !decode(w, r, &req) {
		return
	}
	url, err := s.svc.CreateVendorCheckoutSession(r.PathValue("id"), req.SuccessURL, req.CancelURL)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// vendorClick bumps a vendor's tap-through counter (public, best-effort).
func (s *Server) vendorClick(w http.ResponseWriter, r *http.Request) {
	if !s.regLimiter.allow("vendor-click:" + r.PathValue("id")) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) // silently drop floods
		return
	}
	s.svc.RecordVendorClick(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// courtSponsors returns court number -> sponsoring vendor name (public — the
// TV board reads it).
func (s *Server) courtSponsors(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.CourtSponsors(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// vendorMarkPaid records an off-platform booth payment (owner-only).
func (s *Server) vendorMarkPaid(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.MarkVendorPaid(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paid": true})
}

// createVendor adds a Vendor Village entry to the event (owner-only).
func (s *Server) createVendor(w http.ResponseWriter, r *http.Request) {
	var req model.VendorRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.svc.CreateVendor(r.PathValue("id"), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// updateVendor edits a Vendor Village entry (owner-only).
func (s *Server) updateVendor(w http.ResponseWriter, r *http.Request) {
	var req model.VendorRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.svc.UpdateVendor(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteVendor removes a Vendor Village entry (owner-only).
func (s *Server) deleteVendor(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteVendor(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// notifyVendorDeal pushes an organizer-composed vendor deal to the event's
// players (and posts it to the feed). Owner-only.
func (s *Server) notifyVendorDeal(w http.ResponseWriter, r *http.Request) {
	var req model.VendorNotifyRequest
	if !decode(w, r, &req) {
		return
	}
	n, err := s.svc.NotifyVendorDeal(r.PathValue("id"), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"notified": n})
}

// scoreToken pulls the participant token (?t= / X-Registration-Token) and
// applies the per-match rate limit shared by all score-report actions.
func (s *Server) scoreToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.regLimiter.allow("score:" + r.PathValue("id")) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many requests right now, try again shortly"))
		return "", false
	}
	token := strings.TrimSpace(r.URL.Query().Get("t"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Registration-Token"))
	}
	return token, true
}

// scoreReportState returns the report/confirm page state (participant-only).
func (s *Server) scoreReportState(w http.ResponseWriter, r *http.Request) {
	token, ok := s.scoreToken(w, r)
	if !ok {
		return
	}
	st, err := s.svc.GetScoreReportState(r.PathValue("id"), token, userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// scoreReport records the winning side's score (participant-only).
func (s *Server) scoreReport(w http.ResponseWriter, r *http.Request) {
	token, ok := s.scoreToken(w, r)
	if !ok {
		return
	}
	var req struct {
		Team1Score int `json:"team1Score"`
		Team2Score int `json:"team2Score"`
	}
	if !decode(w, r, &req) {
		return
	}
	st, err := s.svc.ReportScore(r.PathValue("id"), token, userID(r), req.Team1Score, req.Team2Score)
	if err != nil {
		if errors.Is(err, service.ErrScoreReportExists) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

// scoreConfirm finalizes a pending report (opposite side only).
func (s *Server) scoreConfirm(w http.ResponseWriter, r *http.Request) {
	token, ok := s.scoreToken(w, r)
	if !ok {
		return
	}
	st, err := s.svc.ConfirmScore(r.PathValue("id"), token, userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// scoreDispute freezes a pending report for the organizer to resolve.
func (s *Server) scoreDispute(w http.ResponseWriter, r *http.Request) {
	token, ok := s.scoreToken(w, r)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	st, err := s.svc.DisputeScore(r.PathValue("id"), token, req.Note, userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// listScoreReports powers the organizer's match-card chips (owner-only).
func (s *Server) listScoreReports(w http.ResponseWriter, r *http.Request) {
	rows, err := s.svc.ListScoreReports(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// rosterCSV streams the event's registrant roster as a CSV download (owner-only).
func (s *Server) remapCourt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From int `json:"from"`
		To   int `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	moved, err := s.svc.RemapCourt(r.PathValue("id"), req.From, req.To)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"moved": moved})
}

func (s *Server) copyRoster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromEventID string `json:"fromEventId"`
	}
	if !decode(w, r, &req) {
		return
	}
	added, skipped, err := s.svc.CopyRoster(r.PathValue("id"), req.FromEventID, userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added, "skipped": skipped})
}

func (s *Server) rosterCSV(w http.ResponseWriter, r *http.Request) {
	data, err := s.svc.RosterCSV(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="roster.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// importRoster bulk-registers players from a roster import (owner-only).
func (s *Server) importRoster(w http.ResponseWriter, r *http.Request) {
	var req model.ImportRosterRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.svc.ImportRoster(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// importDupr imports a DUPR club's roster into the event (owner-only).
func (s *Server) importDupr(w http.ResponseWriter, r *http.Request) {
	var req model.ImportDuprRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.svc.ImportDuprClubToEvent(
		r.PathValue("id"), req.BracketID, req.DuprClubID)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- Clubs ----

// createClub creates a club (Premium-gated, like createEvent).
func (s *Server) createClub(w http.ResponseWriter, r *http.Request) {
	if !s.svc.IsPremium(userID(r)) {
		writeErr(w, http.StatusPaymentRequired, service.ErrPremiumRequired)
		return
	}
	var req model.CreateClubRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.svc.CreateClub(userID(r), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// updateClub edits a club (owner-only — enforced in the service).
func (s *Server) updateClub(w http.ResponseWriter, r *http.Request) {
	var req model.CreateClubRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.UpdateClub(r.PathValue("id"), userID(r), req); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// getClub returns a club for public viewing (caller flags via optionalAuth).
func (s *Server) getClub(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.GetClub(r.PathValue("id"), userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// myClubs lists the caller's clubs (owned or joined).
func (s *Server) myClubs(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.MyClubs(userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// deleteClub removes a club (owner-only; events survive unlinked).
func (s *Server) deleteClub(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteClub(r.PathValue("id"), userID(r)); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// clubMembers lists a club's members (public).
func (s *Server) clubMembers(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.ClubMembers(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// clubEvents lists a club's events (public).
func (s *Server) clubEvents(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.ClubEvents(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// joinClub adds the caller as a member.
func (s *Server) joinClub(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.JoinClub(r.PathValue("id"), userID(r)); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "joined"})
}

// leaveClub removes the caller's membership.
func (s *Server) leaveClub(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.LeaveClub(r.PathValue("id"), userID(r)); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "left"})
}

// --- Social graph: search players & follow them ---

// searchUsers finds followable accounts by display name (?q=, >= 2 chars).
func (s *Server) searchUsers(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.SearchUsers(userID(r), r.URL.Query().Get("q"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// followUser makes the caller follow the path user.
func (s *Server) followUser(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Follow(userID(r), r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "following"})
}

// unfollowUser removes the caller's follow of the path user.
func (s *Server) unfollowUser(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Unfollow(userID(r), r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unfollowed"})
}

// myFollowing lists the accounts the caller follows.
func (s *Server) myFollowing(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Following(userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// myFollowers lists the accounts that follow the caller.
func (s *Server) myFollowers(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Followers(userID(r))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// uploadTeamBanner uploads an MLP team's banner (owner-only).
func (s *Server) uploadTeamBanner(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 6<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	url, err := s.svc.SetTeamBanner(r.PathValue("id"), r.Header.Get("Content-Type"), data)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bannerUrl": url})
}

// uploadClubLogo uploads a club logo (owner-only — enforced in the service).
func (s *Server) uploadClubLogo(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 6<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	url, err := s.svc.SetClubLogo(r.PathValue("id"), userID(r),
		r.Header.Get("Content-Type"), data)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"logoUrl": url})
}

func (s *Server) addFinanceEntry(w http.ResponseWriter, r *http.Request) {
	var req model.FinanceEntryRequest
	if !decode(w, r, &req) {
		return
	}
	entry, err := s.svc.AddFinanceEntry(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) deleteFinanceEntry(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteFinanceEntry(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) checklist(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Checklist(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) addChecklistItem(w http.ResponseWriter, r *http.Request) {
	var req model.ChecklistItemRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.svc.AddChecklistItem(r.PathValue("id"), req.Label)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) setChecklistChecked(w http.ResponseWriter, r *http.Request) {
	var req model.ChecklistItemRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetChecklistChecked(r.PathValue("id"), req.Checked); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) deleteChecklistItem(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteChecklistItem(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) freebies(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Freebies(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) addFreebie(w http.ResponseWriter, r *http.Request) {
	var req model.FreebieRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.svc.AddFreebie(r.PathValue("id"), req.Name, req.TotalQty)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) updateFreebie(w http.ResponseWriter, r *http.Request) {
	var req model.FreebieRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.svc.UpdateFreebie(r.PathValue("id"), req.Name, req.TotalQty)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) adjustFreebie(w http.ResponseWriter, r *http.Request) {
	var req model.FreebieAdjustRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.svc.AdjustFreebieGiven(r.PathValue("id"), req.Delta)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) deleteFreebie(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteFreebie(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) registrations(w http.ResponseWriter, r *http.Request) {
	regs, err := s.svc.Registrations(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, regs)
}

// busyCourts returns the court numbers that currently have an in-progress
// match, so the schedule UI can gray out other scheduled matches on those
// courts.
func (s *Server) busyCourts(w http.ResponseWriter, r *http.Request) {
	nums, err := s.svc.BusyCourts(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, nums)
}

func (s *Server) eventMatches(w http.ResponseWriter, r *http.Request) {
	// The Game tab's time-grid: pool games, plus elimination-bracket games for
	// single/double-elim & compass (EventScheduleMatches decides per format).
	matches, err := s.svc.EventScheduleMatches(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, matches)
}

func (s *Server) schedule(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"
	// arrange defaults to true; ?arrange=false = a MANUAL build (create games but
	// leave them unplaced for the organizer to position on the Board).
	arrange := r.URL.Query().Get("arrange") != "false"
	res, err := s.svc.GenerateSchedule(r.PathValue("id"), force, arrange)
	if errors.Is(err, service.ErrScheduleHasResults) {
		// 409 — the app should confirm with the user, then retry with ?force=true.
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		status(w, err)
		return
	}
	if res.Matches > 0 {
		s.svc.AddFeedItem(r.PathValue("id"), "schedule_posted",
			fmt.Sprintf("Schedule posted — %d matches", res.Matches), "")
	}
	writeJSON(w, http.StatusOK, res)
}

// manualGame creates one organizer-defined match (the "Add game" dialog).
func (s *Server) manualGame(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BracketID       string   `json:"bracketId"`
		CourtNumber     int      `json:"courtNumber"`
		PlayOrder       int      `json:"playOrder"`
		DurationMinutes int      `json:"durationMinutes"`
		ScheduledDay    *int     `json:"scheduledDay"`
		Team1           []string `json:"team1"`
		Team2           []string `json:"team2"`
	}
	if !decode(w, r, &req) {
		return
	}
	day := -1
	if req.ScheduledDay != nil {
		day = *req.ScheduledDay
	}
	id, err := s.svc.CreateManualGame(r.PathValue("id"), req.BracketID,
		req.CourtNumber, req.PlayOrder, req.DurationMinutes, day, req.Team1, req.Team2)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// --- MLP-style team events ---

func (s *Server) mlpListTeams(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.ListTeams(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) mlpCreateTeam(w http.ResponseWriter, r *http.Request) {
	var req model.CreateTeamRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := s.svc.CreateTeam(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) mlpRemoveTeam(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RemoveEventTeam(r.PathValue("teamId")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) mlpRenameTeam(w http.ResponseWriter, r *http.Request) {
	var req model.CreateTeamRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.RenameEventTeam(r.PathValue("teamId"), req.Name); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) mlpAddTeamMember(w http.ResponseWriter, r *http.Request) {
	var req model.AddTeamMemberRequest
	if !decode(w, r, &req) {
		return
	}
	m, err := s.svc.AddTeamMember(r.PathValue("teamId"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) mlpRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.RemoveTeamMember(r.PathValue("memberId")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) mlpGenerateTies(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.GenerateTeamTies(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"ties": n})
}

func (s *Server) mlpGeneratePlayoff(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.GeneratePlayoff(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"ties": n})
}

func (s *Server) mlpListTies(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.ListTies(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) mlpStandings(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.TeamEventStandings(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) mlpSetLineup(w http.ResponseWriter, r *http.Request) {
	var req model.SetLineupRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetLineLineup(r.PathValue("matchId"), req.Team1, req.Team2); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "set"})
}

func (s *Server) mlpCheckinMember(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CheckedIn bool `json:"checkedIn"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetMemberCheckedIn(r.PathValue("memberId"), req.CheckedIn); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "set"})
}

// clearArrangement un-places every scheduled game (manual scheduling, mode A).
func (s *Server) clearArrangement(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ClearArrangement(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteMatch removes one match + its participants (edit-match sheet's Delete).
func (s *Server) deleteMatch(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteMatch(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// autoSchedule lays the pool games onto courts + time-slots ordered by division
// rating band (lowest first). Owner-only; powers "Build schedule by rating".
func (s *Server) autoSchedule(w http.ResponseWriter, r *http.Request) {
	interleave := r.URL.Query().Get("interleave") == "true"
	// Optional: in PACKED (interleave) mode, keep each player's matches >=
	// minRestSlots time-slots apart. No effect in clean/sequential mode — there
	// divisions already run in separate time blocks. 0 = no gap (default).
	minRest, _ := strconv.Atoi(r.URL.Query().Get("minRestSlots"))
	if minRest < 0 {
		minRest = 0
	}
	n, err := s.svc.AutoScheduleByRating(r.PathValue("id"), interleave, minRest)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"scheduled": n})
}

// setGameDuration updates the per-game slot length (minutes). Owner-only.
func (s *Server) setGameDuration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	m, err := s.svc.SetGameDuration(r.PathValue("id"), req.Minutes)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"minutes": m})
}

// setStartTime sets (or clears) the tournament start (RFC3339 UTC). Owner-only.
func (s *Server) setStartTime(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartsAt string `json:"startsAt"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetStartTime(r.PathValue("id"), req.StartsAt); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "set"})
}

func (s *Server) standings(w http.ResponseWriter, r *http.Request) {
	byWins := r.URL.Query().Get("by") != "points"
	bracketID := r.URL.Query().Get("bracketId")
	st, err := s.svc.Standings(r.PathValue("id"), bracketID, byWins)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) recordScore(w http.ResponseWriter, r *http.Request) {
	var req model.ScoreRequest
	if !decode(w, r, &req) {
		return
	}
	// Per-game scores (best-of-N) when present; otherwise the legacy single game.
	games := req.Games
	if len(games) == 0 {
		games = []model.GameScore{{Team1: req.Team1Score, Team2: req.Team2Score}}
	}
	if err := s.svc.RecordSeries(r.PathValue("id"), games); err != nil {
		status(w, err)
		return
	}
	if eid, txt := s.svc.MatchFeedText(r.PathValue("id"), true); txt != "" {
		s.svc.AddFeedItem(eid, "match_final", txt, r.PathValue("id"))
	}
	// If that final decided a division (the gold final), crown the champions —
	// idempotent (upsert keyed on the final's match id) so a re-score updates the
	// item instead of posting a duplicate / stale wrong winner.
	if eid, txt := s.svc.ChampionFeedText(r.PathValue("id")); txt != "" {
		s.svc.PostChampionFeed(eid, r.PathValue("id"), txt)
	}
	// DUPR submission is queued by advanceAfterScore and flushed by the organizer
	// via "Import to DUPR" (SubmitPendingToDupr) — no extra call here (would
	// double-submit).
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

func (s *Server) forfeitMatch(w http.ResponseWriter, r *http.Request) {
	var req model.ForfeitRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.ForfeitMatch(r.PathValue("id"), req.WinningTeam, req.Kind, req.Team1Score, req.Team2Score); err != nil {
		status(w, err)
		return
	}
	if eid, txt := s.svc.MatchFeedText(r.PathValue("id"), true); txt != "" {
		s.svc.AddFeedItem(eid, "match_final", txt, r.PathValue("id"))
	}
	// If that final decided a division (the gold final), crown the champions —
	// idempotent (upsert keyed on the final's match id) so a re-score updates the
	// item instead of posting a duplicate / stale wrong winner.
	if eid, txt := s.svc.ChampionFeedText(r.PathValue("id")); txt != "" {
		s.svc.PostChampionFeed(eid, r.PathValue("id"), txt)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

func (s *Server) playoff(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TopN    int        `json:"topN"`
		Seeding string     `json:"seeding"`
		Sides   [][]string `json:"sides"`
	}
	// Body is optional (legacy callers passed ?topN= only); tolerate no body.
	// Still cap it (DoS hardening) even though a decode error is ignored here.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.TopN == 0 {
		req.TopN, _ = strconv.Atoi(r.URL.Query().Get("topN"))
	}
	n, err := s.svc.GeneratePlayoffBracket(r.PathValue("id"), req.TopN, req.Seeding, req.Sides)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"matches": n})
}

// playoffSeed returns the division's teams in seed order (with names + record)
// so the Build-playoff dialog can show them for review / manual reordering.
func (s *Server) playoffSeed(w http.ResponseWriter, r *http.Request) {
	seeds, err := s.svc.PlayoffSeedTeams(r.PathValue("id"), r.URL.Query().Get("seeding"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, seeds)
}

func (s *Server) bracketMatches(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.BracketMatches(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// cityAutocomplete returns city suggestions ("City, State") for a free-text club
// or venue city field. Auth-gated to bound the geocoder cost; empty list when no
// geocoder key is configured.
func (s *Server) cityAutocomplete(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "city"
	}
	results := s.svc.PlaceAutocomplete(r.URL.Query().Get("q"), kind)
	if results == nil {
		results = []string{}
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) geocode(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("q query param is required"))
		return
	}
	res, err := s.svc.Geocode(q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if res == nil {
		writeErr(w, http.StatusNotFound, errors.New("location not found"))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) nearbyCourts(w http.ResponseWriter, r *http.Request) {
	lat, err1 := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lng, err2 := strconv.ParseFloat(r.URL.Query().Get("lng"), 64)
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, errors.New("lat and lng query params are required"))
		return
	}
	radius, _ := strconv.ParseFloat(r.URL.Query().Get("radiusKm"), 64)
	found, err := s.svc.NearbyCourts(lat, lng, radius)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, found)
}

func (s *Server) rounds(w http.ResponseWriter, r *http.Request) {
	rs, err := s.svc.Rounds(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (s *Server) roundMatches(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.MatchesForRound(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) pay(w http.ResponseWriter, r *http.Request) {
	var req model.PayRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.authorizeRegistration(w, r, req.Token) {
		return
	}
	ok, err := s.svc.CollectPayment(r.PathValue("id"), req.Provider)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paid": ok})
}

// authorizeRegistration gates the public self-service registration endpoints
// (pay / shirt) against IDOR. It rate-limits per registration id and then
// requires proof of ownership: the event owner's JWT (attached by optionalAuth)
// OR the registration's check_in_token, taken from the bodyToken arg or the
// X-Registration-Token header. Writes the error response and returns false when
// the caller is not allowed.
func (s *Server) authorizeRegistration(w http.ResponseWriter, r *http.Request, bodyToken string) bool {
	regID := r.PathValue("id")
	// Per-registration throttle so a harvested id can't be brute-forced.
	if !s.regLimiter.allow("reg-action:" + regID) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many requests right now, try again shortly"))
		return false
	}
	token := strings.TrimSpace(bodyToken)
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Registration-Token"))
	}
	allowed, err := s.svc.AuthorizeRegistrationAction(regID, token, userID(r))
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, errForbidden)
		return false
	}
	return true
}

// markPaid is the organizer confirming, owner-only, that a fee-bearing
// registration was paid out of band (cash/e-transfer). The public /pay endpoint
// cannot do this for fee-bearing events (no real gateway is wired up).
func (s *Server) markPaid(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.CollectPaymentManually(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paid": true})
}

// stripeConnect starts (or resumes) the authenticated organizer's Stripe
// Connect onboarding and returns a Stripe-hosted account-link URL to redirect
// to. requireAuth scopes it to the caller; the account is keyed by their user id.
func (s *Server) stripeConnect(w http.ResponseWriter, r *http.Request) {
	var req model.StripeConnectRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ReturnURL) == "" || strings.TrimSpace(req.RefreshURL) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("returnUrl and refreshUrl are required"))
		return
	}
	url, err := s.svc.StripeConnectStart(userID(r), req.ReturnURL, req.RefreshURL)
	if errors.Is(err, service.ErrPaymentsNotConfigured) {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, model.URLResponse{URL: url})
}

// subscribePremium opens a Stripe subscription Checkout for the Premium plan.
func (s *Server) subscribePremium(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SuccessURL string `json:"successUrl"`
		CancelURL  string `json:"cancelUrl"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SuccessURL) == "" || strings.TrimSpace(req.CancelURL) == "" {
		writeErr(w, http.StatusBadRequest,
			errors.New("successUrl and cancelUrl are required"))
		return
	}
	url, err := s.svc.StartPremiumCheckout(
		userID(r), userEmail(r), req.SuccessURL, req.CancelURL)
	if errors.Is(err, service.ErrPaymentsNotConfigured) {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, model.URLResponse{URL: url})
}

// startEventPassCheckout opens a one-time Stripe Checkout for the per-event
// Premium pass. The route's ownerOnly wrapper has already verified the caller
// owns {id}; the webhook flips events.premium_pass on success.
func (s *Server) startEventPassCheckout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SuccessURL string `json:"successUrl"`
		CancelURL  string `json:"cancelUrl"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SuccessURL) == "" || strings.TrimSpace(req.CancelURL) == "" {
		writeErr(w, http.StatusBadRequest,
			errors.New("successUrl and cancelUrl are required"))
		return
	}
	url, err := s.svc.StartEventPassCheckout(
		r.PathValue("id"), userEmail(r), req.SuccessURL, req.CancelURL)
	if errors.Is(err, service.ErrPaymentsNotConfigured) {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, model.URLResponse{URL: url})
}

// subscriptionStatus reports the caller's Premium plan state.
func (s *Server) subscriptionStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.GetPremiumStatus(userID(r)))
}

// billingPortal opens the Stripe billing portal for the caller to manage/cancel.
func (s *Server) billingPortal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ReturnURL string `json:"returnUrl"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ReturnURL) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("returnUrl is required"))
		return
	}
	url, err := s.svc.BillingPortal(userID(r), req.ReturnURL)
	if errors.Is(err, service.ErrPaymentsNotConfigured) {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, model.URLResponse{URL: url})
}

// stripeStatus reports the authenticated organizer's Stripe Connect state
// (connected + chargesEnabled), refreshed from Stripe when an account exists.
func (s *Server) stripeStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.StripeAccountStatus(userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, model.StripeStatusResponse{
		Connected:      st.Connected,
		ChargesEnabled: st.ChargesEnabled,
	})
}

// checkout opens a Stripe Checkout Session for a registration's entry fee and
// returns the hosted Checkout URL. Same IDOR guard as /pay (token or owner JWT).
func (s *Server) checkout(w http.ResponseWriter, r *http.Request) {
	var req model.CheckoutRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.authorizeRegistration(w, r, req.Token) {
		return
	}
	if strings.TrimSpace(req.SuccessURL) == "" || strings.TrimSpace(req.CancelURL) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("successUrl and cancelUrl are required"))
		return
	}
	url, err := s.svc.CreateCheckoutSession(r.PathValue("id"), req.SuccessURL, req.CancelURL)
	if errors.Is(err, service.ErrPaymentsNotConfigured) || errors.Is(err, service.ErrOrganizerNotConnected) {
		// Payments not available for this event yet — the client should fall back
		// to the existing manual/pending flow.
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.URLResponse{URL: url})
}

// stripeWebhook handles Stripe's server-to-server callbacks. NO auth wrapper —
// it's authenticated by the Stripe-Signature header verified against
// STRIPE_WEBHOOK_SECRET. It reads the RAW body (the signature is computed over
// the exact bytes), capped for DoS safety, and never routes through decode().
func (s *Server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("could not read request body"))
		return
	}
	if err := s.svc.HandleStripeWebhook(payload, r.Header.Get("Stripe-Signature")); err != nil {
		if errors.Is(err, service.ErrPaymentsNotConfigured) {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
		// A verification/processing failure is a 400 so Stripe retries.
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// updateRegistrationDetails edits a registered player's name/rating (owner-only).
func (s *Server) updateRegistrationDetails(w http.ResponseWriter, r *http.Request) {
	var req model.RegistrationDetailsRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.UpdateRegistrationDetails(r.PathValue("id"), req.FullName, req.DuprRating); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// setPartner pairs a doubles registration with a partner (a registered player
// via partnerRegistrationId, or a free-text partnerName), or clears it when both
// are empty. Returns scheduleStale=true when a schedule already exists and a
// real pairing changed (so the client can prompt a regenerate).
func (s *Server) setPartner(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PartnerRegistrationID string `json:"partnerRegistrationId"`
		PartnerName           string `json:"partnerName"`
	}
	if !decode(w, r, &req) {
		return
	}
	stale, err := s.svc.SetPartner(r.PathValue("id"), req.PartnerRegistrationID, req.PartnerName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"scheduleStale": stale})
}

// deleteRegistration removes a player's registration from an event (owner-only).
func (s *Server) deleteRegistration(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteRegistration(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) deleteRound(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteRound(r.PathValue("id")); err != nil {
		if errors.Is(err, service.ErrScheduleHasResults) {
			http.Error(w,
				"this round has scored matches — remove the scores first",
				http.StatusConflict)
			return
		}
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) duprStatuses(w http.ResponseWriter, r *http.Request) {
	rows, err := s.svc.DuprSubmissionStatuses(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) checkin(w http.ResponseWriter, r *http.Request) {
	var req model.CheckinRequest
	if !decode(w, r, &req) {
		return
	}
	changed, err := s.svc.CheckIn(r.PathValue("id"), req.Method)
	if err != nil {
		status(w, err)
		return
	}
	if changed {
		if eid, txt := s.svc.CheckinFeedText(r.PathValue("id")); txt != "" {
			s.svc.AddFeedItem(eid, "checked_in", txt, r.PathValue("id"))
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "checked_in"})
}

func (s *Server) uncheckin(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.UncheckIn(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setAddons records a registrant's paid add-on choices (tee / overgrips) —
// token- or owner-gated like /pay and /shirt; charged with the entry fee.
func (s *Server) setAddons(w http.ResponseWriter, r *http.Request) {
	var req model.AddonsRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.authorizeRegistration(w, r, req.Token) {
		return
	}
	if err := s.svc.SetRegistrationAddons(r.PathValue("id"), req.Tee, req.Grips); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveShirt(w http.ResponseWriter, r *http.Request) {
	var req model.ShirtRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.authorizeRegistration(w, r, req.Token) {
		return
	}
	order, err := s.svc.SaveShirtOrder(r.PathValue("id"), req)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) checkinByToken(w http.ResponseWriter, r *http.Request) {
	var req model.TokenRequest
	if !decode(w, r, &req) {
		return
	}
	regID, err := s.svc.CheckInByToken(r.PathValue("id"), req.Token)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"registrationId": regID})
}

func (s *Server) checkinByPhone(w http.ResponseWriter, r *http.Request) {
	var req model.PhoneCheckinRequest
	if !decode(w, r, &req) {
		return
	}
	// Throttle this lookup PER EVENT (not per IP): behind Railway's proxy chain a
	// client IP isn't reliably attributable — the leftmost X-Forwarded-For hop is
	// client-spoofable and the rightmost can be a rotating internal hop — so an IP
	// key is either bypassable or explodes. A per-event cap bounds total attempts
	// regardless and, with the exact full-phone match this endpoint requires (the
	// real anti-enumeration control), is enough to stop name harvesting. QR and
	// organizer check-in are separate endpoints and unaffected.
	if !s.phoneCheckin.allow(r.PathValue("id")) {
		writeErr(w, http.StatusTooManyRequests, errors.New("too many attempts, try again shortly"))
		return
	}
	regID, name, err := s.svc.CheckInByPhone(r.PathValue("id"), req.Phone)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"registrationId": regID, "name": name})
}

func (s *Server) swapMatchPlayer(w http.ResponseWriter, r *http.Request) {
	var req model.SwapRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SwapMatchPlayer(r.PathValue("id"), req.OutPlayerID, req.InPlayerID); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "swapped"})
}

// swapMatchPlayersCross exchanges two players who sit in two DIFFERENT matches.
// Because it spans two matches, ownerOnly (single path id) doesn't fit: it's
// requireAuth-gated and verifies the caller owns BOTH matches' events here.
func (s *Server) swapMatchPlayersCross(w http.ResponseWriter, r *http.Request) {
	var req model.SwapCrossRequest
	if !decode(w, r, &req) {
		return
	}
	if req.MatchA == "" || req.MatchB == "" {
		writeErr(w, http.StatusBadRequest, errors.New("matchA and matchB are required"))
		return
	}
	uid := userID(r)
	for _, mid := range []string{req.MatchA, req.MatchB} {
		owner, err := s.svc.OwnerOf("match", mid)
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if owner == "" || owner != uid {
			writeErr(w, http.StatusForbidden, errForbidden)
			return
		}
	}
	warning, err := s.svc.SwapPlayersAcrossMatches(req.MatchA, req.PlayerA, req.MatchB, req.PlayerB)
	if err != nil {
		status(w, err)
		return
	}
	resp := map[string]any{"status": "swapped"}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// setMatchCourt reassigns a match to a different court (courtNumber <= 0 = clear
// the assignment). Owner-only; powers the drag-to-reassign schedule board.
func (s *Server) setMatchCourt(w http.ResponseWriter, r *http.Request) {
	var req model.SetCourtRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetMatchCourt(r.PathValue("id"), req.CourtNumber, req.PlayOrder); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "court-set"})
}

// setMatchDuration overrides one game's length (minutes); 0 clears it. Owner-only.
func (s *Server) setMatchDuration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	m, err := s.svc.SetMatchDuration(r.PathValue("id"), req.Minutes)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"minutes": m})
}

// setEventBreaks replaces an event's blocked time ranges (lunch, etc.). Owner-only.
func (s *Server) setEventBreaks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Breaks []model.ScheduleBreak `json:"breaks"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetEventBreaks(r.PathValue("id"), req.Breaks); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"breaks": len(req.Breaks)})
}

// setDayCap sets the day cap (latest start time-of-day, minutes). Owner-only.
func (s *Server) setDayCap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DayCapMinutes int `json:"dayCapMinutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetDayCap(r.PathValue("id"), req.DayCapMinutes); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"dayCapMinutes": req.DayCapMinutes})
}

// setDayEnds sets per-day court closing times (minutes from midnight, indexed by
// tournament day; -1 = no close that day). Owner-only.
func (s *Server) setDayEnds(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DayEndMinutes []int `json:"dayEndMinutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetDayEnds(r.PathValue("id"), req.DayEndMinutes); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]int{"dayEndMinutes": req.DayEndMinutes})
}

// setMatchDay assigns a match to a tournament day (0-based); a negative day
// clears it (falls back to the auto split). Owner-only.
func (s *Server) setMatchDay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Day int `json:"day"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.svc.SetMatchDay(r.PathValue("id"), req.Day); err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"day": req.Day})
}

func (s *Server) startRound(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.StartRound(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}

func (s *Server) startMatch(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.StartMatch(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	if eid, txt := s.svc.MatchFeedText(r.PathValue("id"), false); txt != "" {
		s.svc.AddFeedItem(eid, "match_live", txt, r.PathValue("id"))
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}

// nearbyEvents lists publicly-listed events sorted by distance from ?lat&lng,
// paginated 10 per ?page (0-based). Public — anyone can discover open events.
func (s *Server) nearbyEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, errLat := strconv.ParseFloat(q.Get("lat"), 64)
	lng, errLng := strconv.ParseFloat(q.Get("lng"), 64)
	if errLat != nil || errLng != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("lat and lng are required"))
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	events, err := s.svc.NearbyEvents(lat, lng, page, 10)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// shortLink 302-redirects an SMS short code to its stored full URL.
func (s *Server) shortLink(w http.ResponseWriter, r *http.Request) {
	target, err := s.svc.ResolveShortLink(r.PathValue("code"))
	if err != nil || target == "" {
		http.Error(w, "link not found", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// publicEvents serves the planmypickle.com marketing feed: up to 20 recent /
// upcoming publicly-listed events in a safe, PII-free projection. No auth — it's
// read cross-origin from the apex marketing site.
func (s *Server) publicEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.svc.PublicEvents(20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// roster returns the public player list (names + division + check-in status).
func (s *Server) roster(w http.ResponseWriter, r *http.Request) {
	entries, err := s.svc.Roster(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) playerProfile(w http.ResponseWriter, r *http.Request) {
	prof, err := s.svc.PlayerProfile(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prof)
}

// feedList returns an event's activity feed (public — like the scoreboard).
// optionalAuth so a signed-in caller's own reactions come back flagged.
func (s *Server) feedList(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.ListFeed(r.PathValue("id"), userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// feedReact toggles the signed-in user's reaction on a feed item.
func (s *Server) feedReact(w http.ResponseWriter, r *http.Request) {
	var req model.ReactionRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.svc.ToggleReaction(r.PathValue("id"), userID(r), req.Type)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// commentList returns a feed item's comments (public read; canDelete/mine
// reflect the optional caller).
func (s *Server) commentList(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.ListComments(r.PathValue("id"), userID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// commentAdd posts a comment as the signed-in user.
func (s *Server) commentAdd(w http.ResponseWriter, r *http.Request) {
	var req model.CommentRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.svc.AddComment(r.PathValue("id"), userID(r), userEmail(r), req.Text)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// commentDelete removes a comment (author or event owner).
func (s *Server) commentDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteComment(r.PathValue("id"), userID(r)); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// feedPost adds an organizer announcement to the feed (owner-only).
func (s *Server) feedPost(w http.ResponseWriter, r *http.Request) {
	var req model.FeedPostRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.svc.PostAnnouncement(r.PathValue("id"), req.Text, "Organizer")
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

// feedDelete removes a feed item (owner-only).
func (s *Server) feedDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteFeedItem(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fillRandomPlayers seeds the event with a day's worth of demo players spread
// across its divisions (owner-only). Temporary organizer convenience.
func (s *Server) fillRandomPlayers(w http.ResponseWriter, r *http.Request) {
	// Optional ?bracket=<id> seeds only that division (the one the organizer is
	// viewing in the Players tab); empty spreads across all divisions.
	n, err := s.svc.FillRandomPlayers(r.PathValue("id"), r.URL.Query().Get("bracket"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": n})
}

// registerDuprTesters enrolls the fixed DUPR UAT test accounts into the event
// (demo helper) and returns a summary of what happened.
func (s *Server) registerDuprTesters(w http.ResponseWriter, r *http.Request) {
	sum, err := s.svc.RegisterDuprTestAccounts(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// removeFromDupr reverses the event's submitted results on DUPR (delete leg).
func (s *Server) removeFromDupr(w http.ResponseWriter, r *http.Request) {
	sum, err := s.svc.RemoveEventFromDupr(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// removeMatchFromDupr reverses ONE match's submitted result on DUPR (per-game
// delete leg) — leaves the local match + score, just un-submits it from DUPR.
func (s *Server) removeMatchFromDupr(w http.ResponseWriter, r *http.Request) {
	sum, err := s.svc.RemoveMatchFromDupr(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) unstartMatch(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.UnstartMatch(r.PathValue("id")); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) duprImport(w http.ResponseWriter, r *http.Request) {
	sum, err := s.svc.SubmitPendingToDupr(r.PathValue("id"))
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) verifyAdmin(w http.ResponseWriter, r *http.Request) {
	var req model.PasscodeRequest
	if !decode(w, r, &req) {
		return
	}
	ok, err := s.svc.VerifyAdminPasscode(r.PathValue("id"), req.Code)
	if err != nil {
		status(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": ok})
}

// ---- helpers ----

var errForbidden = errors.New("you don't have access to this resource")

// ownerOnly guards a handler so only the authenticated owner of the event
// behind a resource may call it. kind/idParam identify the resource (see
// service.OwnerOf); idParam is the path wildcard holding its id. Unowned
// (legacy) resources are treated as forbidden — nobody may mutate them.
func (s *Server) ownerOnly(kind, idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.OwnerOf(kind, r.PathValue(idParam))
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if owner == "" || owner != userID(r) {
			writeErr(w, http.StatusForbidden, errForbidden)
			return
		}
		next(w, r)
	})
}

// leagueViewer guards a league READ handler keyed on a league path id so it's
// reachable by the league's OWNER or a PARTICIPANT (registered for one of its
// sessions, or an entrant in one of its brackets — service.IsLeagueParticipant).
// READ-ONLY: every league WRITE stays behind ownerOnly. Mirrors ownerOnly's
// shape but widens the allow check from owner-only to owner-OR-participant.
func (s *Server) leagueViewer(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowLeagueRead(w, r, r.PathValue(idParam)) {
			return
		}
		next(w, r)
	})
}

// leagueBracketViewer is leagueViewer for the ladder/team/flex READ handlers,
// which are keyed on a division (league_bracket) id: it resolves the division to
// its league, then applies the same owner-OR-participant check. READ-ONLY — the
// matching WRITE handlers stay on the owner guards (ladder/team/flexDivisionOwner).
func (s *Server) leagueBracketViewer(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		leagueID, err := s.svc.LeagueIDOfDivision(r.PathValue(idParam))
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !s.allowLeagueRead(w, r, leagueID) {
			return
		}
		next(w, r)
	})
}

// allowLeagueRead reports whether the authenticated caller may READ the given
// league: true when they own it OR participate in it. It writes the appropriate
// error response (404 for a missing league, 403 otherwise) and returns false
// when access is denied. Shared by leagueViewer and leagueBracketViewer.
func (s *Server) allowLeagueRead(w http.ResponseWriter, r *http.Request, leagueID string) bool {
	owner, err := s.svc.OwnerOf("league", leagueID)
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	uid := userID(r)
	if uid != "" && owner == uid {
		return true
	}
	ok, err := s.svc.IsLeagueParticipant(leagueID, uid, userEmail(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if !ok {
		writeErr(w, http.StatusForbidden, errForbidden)
		return false
	}
	return true
}

// ladderDivisionOwner guards a ladder handler keyed on a league_bracket
// (division) path id: it requires a valid token AND that the caller owns the
// league behind that division. Mirrors ownerOnly, but resolves ownership via the
// division → league → owner chain (service.LadderOwner).
func (s *Server) ladderDivisionOwner(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.LadderOwner(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// ladderEntrantOwner is ladderDivisionOwner for handlers keyed on an entrant id
// (resolves entrant → division → league → owner via service.LadderOwnerOfEntrant).
func (s *Server) ladderEntrantOwner(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.LadderOwnerOfEntrant(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// teamDivisionOwner guards a team-league handler keyed on a league_bracket
// (division) path id: it requires a valid token AND that the caller owns the
// league behind that division (division → league → owner via service.TeamOwner).
// Mirrors ladderDivisionOwner.
func (s *Server) teamDivisionOwner(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.TeamOwner(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// teamOwnerOfTeam is teamDivisionOwner for handlers keyed on a team id (resolves
// team → division → league → owner via service.TeamOwnerOfTeam).
func (s *Server) teamOwnerOfTeam(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.TeamOwnerOfTeam(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// flexDivisionOwner guards a Flex-league handler keyed on a league_bracket
// (division) path id: it requires a valid token AND that the caller owns the
// league behind that division (division → league → owner via service.FlexOwner).
// Mirrors teamDivisionOwner.
func (s *Server) flexDivisionOwner(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.FlexOwner(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// flexOwnerOfTeam is flexDivisionOwner for handlers keyed on a team id (resolves
// team → division → league → owner via service.TeamOwnerOfTeam — Flex reuses the
// `teams` table, so the same ownership chain applies).
func (s *Server) flexOwnerOfTeam(idParam string, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		owner, err := s.svc.TeamOwnerOfTeam(r.PathValue(idParam))
		if !ladderOwnerOK(w, r, owner, err) {
			return
		}
		next(w, r)
	})
}

// ladderOwnerOK writes the appropriate error and returns false when the owner
// lookup failed or the caller isn't the owner; true means proceed.
func ladderOwnerOK(w http.ResponseWriter, r *http.Request, owner string, err error) bool {
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if owner == "" || owner != userID(r) {
		writeErr(w, http.StatusForbidden, errForbidden)
		return false
	}
	return true
}

// ownerOrPasscode guards a handler so it's reachable by EITHER the event owner
// (a valid JWT, exactly like ownerOnly) OR a client holding the event's admin
// passcode in the X-Event-Passcode header. This is the scorekeeper path: an
// organizer hands a volunteer the passcode so they can record scores without a
// full account/login.
//
// kind/idParam identify the resource (currently only "match"); the passcode is
// validated against THAT resource's event via VerifyAdminPasscode. Rules: a
// valid owner JWT always wins; otherwise a correct passcode for the match's
// event is required; anything else is 403. A missing/blank passcode with no
// owner JWT is rejected.
func (s *Server) ownerOrPasscode(kind, idParam string, next http.HandlerFunc) http.HandlerFunc {
	return optionalAuth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue(idParam)
		// Owner path: same check as ownerOnly. A valid JWT that owns the event wins.
		owner, err := s.svc.OwnerOf(kind, id)
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if uid := userID(r); uid != "" && owner != "" && owner == uid {
			next(w, r)
			return
		}
		// Passcode path: require a non-blank X-Event-Passcode, validated against the
		// resource's event. (Only "match" is wired today.)
		code := strings.TrimSpace(r.Header.Get("X-Event-Passcode"))
		if code == "" || kind != "match" {
			writeErr(w, http.StatusForbidden, errForbidden)
			return
		}
		eventID, err := s.svc.EventIDOfMatch(id)
		if errors.Is(err, service.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		ok, err := s.svc.VerifyAdminPasscode(eventID, code)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusForbidden, errForbidden)
			return
		}
		next(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// Cap the request body (DoS hardening): a JSON payload over 1 MiB is rejected
	// rather than buffered. No legitimate request here is that large.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// Tolerate an empty body so optional-field endpoints (pay, checkin) work
	// with no JSON; fields fall back to their service-side defaults.
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func status(w http.ResponseWriter, err error) {
	if errors.Is(err, service.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, service.ErrForbidden) {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	if errors.Is(err, service.ErrPremiumRequired) {
		writeErr(w, http.StatusPaymentRequired, err)
		return
	}
	writeErr(w, http.StatusBadRequest, err)
}

// rateLimiter is a small in-memory sliding-window limiter (best-effort; per
// process, fine for a single backend instance). It throttles the public phone
// check-in endpoint against name-enumeration brute force.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]int64
	limit  int
	window int64 // seconds
}

func newRateLimiter(limit int, windowSec int64) *rateLimiter {
	rl := &rateLimiter{hits: map[string][]int64{}, limit: limit, window: windowSec}
	go rl.sweep()
	return rl
}

// sweep periodically drops keys whose newest attempt is older than the window,
// so the map can't grow without bound (one entry per IP+event would otherwise
// live for the whole process — and an attacker varying the key could pile them
// up). Runs for the process lifetime.
func (rl *rateLimiter) sweep() {
	for range time.Tick(time.Duration(rl.window) * time.Second) {
		cutoff := time.Now().Unix() - rl.window
		rl.mu.Lock()
		for k, ts := range rl.hits {
			newest := int64(0)
			for _, t := range ts {
				if t > newest {
					newest = t
				}
			}
			if newest <= cutoff {
				delete(rl.hits, k)
			}
		}
		rl.mu.Unlock()
	}
}

// allow records an attempt for key and reports whether it's within the limit.
func (rl *rateLimiter) allow(key string) bool {
	nowTs := time.Now().Unix()
	cutoff := nowTs - rl.window
	rl.mu.Lock()
	defer rl.mu.Unlock()
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, nowTs)
	return true
}

// corsAllowedOrigins are the first-party browser origins that read this API:
// the Flutter app, the apex marketing site (planmypickle.com — the public
// tournaments feed), and localhost for dev. Vercel preview deploys (*.vercel.app)
// are also honored so pre-production testing against this API keeps working.
// The API carries no cookies/credentials (auth is a bearer token JS must attach,
// never auto-sent), so this restriction is defense-in-depth: it stops a random
// third-party site from driving the API from a signed-in user's browser.
var corsAllowedOrigins = []string{
	"https://app.planmypickle.com", // Flutter app
	"https://planmypickle.com",     // apex marketing site (public feed)
	"https://www.planmypickle.com", // www marketing site (public feed)
	"http://localhost:3000",
	"http://localhost:8080",
}

// corsOriginAllowed reports whether an Origin header should be reflected back.
func corsOriginAllowed(origin string) bool {
	for _, o := range corsAllowedOrigins {
		if origin == o {
			return true
		}
	}
	// Vercel preview deployments of the app (e.g. app-git-branch-xyz.vercel.app).
	return strings.HasSuffix(origin, ".vercel.app")
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reflect the caller's origin only when it's an allow-listed first-party
		// origin — not a blanket "*". The marketing feed and spectator reads come
		// from planmypickle.com / the app / preview deploys, all covered above.
		if origin := r.Header.Get("Origin"); corsOriginAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// Vary so a shared cache never serves one origin's ACAO to another.
			w.Header().Add("Vary", "Origin")
		}
		// DELETE is used by events/finance/checklist; without it a browser's
		// preflight blocks those calls.
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		// Authorization carries the user's bearer token; without it the browser
		// preflight blocks every authenticated request. X-Registration-Token
		// (registrant self-service pay/shirt) and X-Event-Passcode (scorekeeper
		// auth) are custom headers, so they must be allow-listed too.
		w.Header().Set("Access-Control-Allow-Headers",
			"Content-Type,Authorization,X-Registration-Token,X-Event-Passcode")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
