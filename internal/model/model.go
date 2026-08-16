// Package model holds the API/domain structs shared by the service and HTTP layers.
package model

type Event struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Format              string `json:"format"`           // singles | doubles
	PartnerMode         string `json:"partnerMode"`      // fixed | rotating | na
	TournamentFormat    string `json:"tournamentFormat"` // round_robin | single_elim | pools_playoff
	ScoringMode         string `json:"scoringMode"`      // points | wins
	NumCourts           int    `json:"numCourts"`
	PointsToWin         int    `json:"pointsToWin"`
	WinBy               int    `json:"winBy"`
	BestOf              int    `json:"bestOf"` // games per match: 1 (single) or 3 (best of 3)
	GameDurationMinutes int    `json:"gameDurationMinutes"`
	// MinPoolRounds / MaxPoolRounds bound the pool-play round-robin length
	// (0 = unset). Max caps a full RR (partial round-robin); min tops it up by
	// repeating matchups so everyone gets a guaranteed number of games.
	MinPoolRounds int `json:"minPoolRounds"`
	MaxPoolRounds int `json:"maxPoolRounds"`
	// RoundsPerSession pins an EXACT number of rounds generated each time the
	// schedule is built (0 = unset → derive per format). Overrides the min/max
	// bounds above when > 0. Mainly for recurring leagues that want a fixed
	// "N rounds a week".
	RoundsPerSession int `json:"roundsPerSession"`
	// RsvpEnabled turns on the "Playing Thursday?" RSVP strip on a recurring
	// league's feed (organizer opt-in).
	RsvpEnabled          bool `json:"rsvpEnabled"`
	RegistrationFeeCents int  `json:"registrationFeeCents"`
	// Multi-division pricing: how the entry fee applies to a player's ADDITIONAL
	// divisions (their first division always pays the full fee). "discount"
	// (default) charges AdditionalDivisionFeeCents; "free" charges 0; "full"
	// charges the full fee again.
	ExtraDivisionFeeMode       string `json:"extraDivisionFeeMode"`
	AdditionalDivisionFeeCents int    `json:"additionalDivisionFeeCents"`
	// Paid add-ons a registrant can buy with their entry (0 = not offered).
	AddonTeeCents   int `json:"addonTeeCents"`
	AddonGripsCents int `json:"addonGripsCents"`
	// Event-tee presale (custom merch): a name, up to two design images
	// (front/back), and the sizes the organizer offers. Priced by AddonTeeCents.
	AddonTeeName     string   `json:"addonTeeName,omitempty"`
	AddonTeeFrontURL string   `json:"addonTeeFrontUrl,omitempty"`
	AddonTeeBackURL  string   `json:"addonTeeBackUrl,omitempty"`
	AddonTeeSizes    []string `json:"addonTeeSizes,omitempty"`
	Currency         string   `json:"currency"`
	ZelleHandle      *string  `json:"zelleHandle,omitempty"`
	VenmoHandle      *string  `json:"venmoHandle,omitempty"`
	// GcashHandle = the organizer's GCash mobile number for manual PH collection
	// (players pay out-of-band, organizer marks paid). Same model as Zelle/Venmo.
	GcashHandle  *string `json:"gcashHandle,omitempty"`
	ClubID       *string `json:"clubId,omitempty"`
	Location     *string `json:"location,omitempty"`
	ContactPhone *string `json:"contactPhone,omitempty"`
	VenueNotes   *string `json:"venueNotes,omitempty"`
	WaiverURL    *string `json:"waiverUrl,omitempty"`
	// Optional organizer-customized registration-confirmation email. Subject
	// overrides the default "You're in! …"; Message is a personal note added to
	// the top of the email. Both empty/unset → the branded default.
	ConfirmEmailSubject *string `json:"confirmEmailSubject,omitempty"`
	ConfirmEmailMessage *string `json:"confirmEmailMessage,omitempty"`
	// Organizer email branding (Premium) — see CreateEventRequest for semantics.
	EmailBrandLogoURL string `json:"emailBrandLogoUrl,omitempty"`
	EmailBrandColor   string `json:"emailBrandColor,omitempty"`
	EmailSignature    string `json:"emailSignature,omitempty"`
	// RatingEnforcement gates self-registration against a division's DUPR band:
	// "off" (default) | "warn" (flag out-of-band) | "block" (reject out-of-band).
	RatingEnforcement string   `json:"ratingEnforcement,omitempty"`
	VenueName         *string  `json:"venueName,omitempty"`
	VenueAddress      *string  `json:"venueAddress,omitempty"`
	VenuePhone        *string  `json:"venuePhone,omitempty"`
	VenueWebsite      *string  `json:"venueWebsite,omitempty"`
	VenueLat          *float64 `json:"venueLat,omitempty"`
	VenueLng          *float64 `json:"venueLng,omitempty"`
	// County + state resolved from the venue coords — used to filter the public
	// Nearby feed to the requester's own county (matched together, since county
	// names collide across states). Best-effort; "" when unresolved.
	City           string `json:"city,omitempty"`
	County         string `json:"county,omitempty"`
	State          string `json:"state,omitempty"`
	DuprSanctioned bool   `json:"duprSanctioned"`
	// DuprMinEntitlement, when set to "DUPR_PLUS", gates self-registration on a
	// DUPR+ membership — the player must hold BOTH the PREMIUM_L1 and VERIFIED_L1
	// entitlements (DUPR's one consumer-facing tier). Empty = a standard
	// sanctioned event (any connected DUPR account). Setting this implies
	// DuprSanctioned.
	DuprMinEntitlement string `json:"duprMinEntitlement,omitempty"`
	// CashPrize flags a cash-prize event; CashPrizeAmount is the optional pot size.
	CashPrize       bool     `json:"cashPrize"`
	CashPrizeAmount *float64 `json:"cashPrizeAmount,omitempty"`
	// Consolation enables a consolation back-draw for single_elim: first-round
	// losers play down to a consolation champion / bronze, so every team that
	// plays round 1 gets ≥2 matches (USAP 12.J). Note: a round-1-bye seed that
	// loses its first real match is not back-drawn (see engine/consolation.go).
	Consolation bool `json:"consolation"`
	// AutoAdjust re-flows later match start times to follow ACTUAL game finishes
	// (early or late) as scores come in, instead of the fixed planned slots.
	AutoAdjust bool `json:"autoAdjust"`
	// AutoStartNext: when a game finishes (both sides' score recorded/confirmed),
	// auto-start the next scheduled game waiting on that freed court.
	AutoStartNext bool `json:"autoStartNext"`
	// CourtCalls: the TV/kiosk scoreboard defaults its PA-style voice court-call
	// toggle ON for this event (device still needs one tap to unlock audio).
	CourtCalls bool `json:"courtCalls"`
	// TeamSize > 0 marks an MLP-style team event (roster size per team; 4 = 2M/2W).
	TeamSize int `json:"teamSize"`
	// StartsAt is the scheduled tournament start (RFC3339 UTC), or nil.
	StartsAt *string `json:"startsAt,omitempty"`
	// NextSessionAt is the next occurrence for a PERPETUAL league, RFC3339.
	//
	// A perpetual league is one long-running event: its StartsAt is the day the
	// league began and never moves. Read literally that makes it permanently
	// in-progress ("Happening now", forever) and shows a date months past. This
	// is the date such a league should actually present. Nil for everything
	// else, where StartsAt already means what it says.
	NextSessionAt *string `json:"nextSessionAt,omitempty"`
	// EndsAt is the scheduled end (RFC3339 UTC), or nil — for multi-day events.
	EndsAt      *string `json:"endsAt,omitempty"`
	Description *string `json:"description,omitempty"`
	// RegisteredCount is the number of registration ENTRIES (a player in 2
	// divisions counts twice); filled on the dashboard list + single reads.
	RegisteredCount int `json:"registeredCount"`
	// DistinctPlayerCount is unique players (dedup by player_id); filled on single
	// event reads. Use this (not RegisteredCount) to compare against MaxPlayers.
	DistinctPlayerCount int `json:"distinctPlayerCount"`
	// CheckedInCount is how many of the registered players are checked in
	// (filled on the event-detail read).
	CheckedInCount int    `json:"checkedInCount"`
	Status         string `json:"status"`
	// LiveCount is the number of matches currently in progress (filled on the
	// dashboard/playing lists so cards can show a "live" pill).
	LiveCount int `json:"liveCount"`
	// LastActivity* mirror the newest feed item for this event (filled on the
	// list endpoints) so a home card can preview recent activity.
	LastActivity     *string `json:"lastActivity,omitempty"`
	LastActivityType *string `json:"lastActivityType,omitempty"`
	LastActivityAt   *string `json:"lastActivityAt,omitempty"`
	// Listed = organizer opted this event into the public "Nearby" discovery feed.
	Listed bool `json:"listed"`
	// PlayerScoring = the Premium "Player Score Confirm" add-on: winners report
	// scores from their token links; losers confirm/dispute; auto-confirm after
	// ScoreConfirmMinutes. Organizer scoring always overrides.
	PlayerScoring       bool `json:"playerScoring"`
	ScoreConfirmMinutes int  `json:"scoreConfirmMinutes"`
	// SmsNotifications = premium "both channels": when true (and the owner is
	// premium), automated alerts (court calls, delay updates) ALSO go by SMS on
	// top of push. Default false = push-first (free). Gated at the API.
	SmsNotifications bool `json:"smsNotifications"`
	// OnDeckSms = also text the on-deck warm-up heads-up (needs SmsNotifications).
	// Default false — on-deck stays push-only unless the organizer opts in, since
	// it ~doubles court-call SMS volume.
	OnDeckSms bool `json:"onDeckSms"`
	// MaxPlayers caps how many distinct players may register for the event; nil or
	// <=0 = unlimited. Enforced on self-registration (an organizer adding players
	// can still exceed it).
	MaxPlayers *int `json:"maxPlayers,omitempty"`
	// RequireApproval holds every self-registration as pending (approved=false)
	// until the organizer approves it — pending entries stay out of the roster
	// counts and the draw. Organizer-added players are always approved.
	RequireApproval bool `json:"requireApproval"`
	// CourtScorePasscode reports whether court-QR scoring REQUIRES the scorekeeper
	// passcode (the per-event setting is on AND a passcode is set). Derived so the
	// court page shows the passcode gate exactly when the backend enforces it; the
	// passcode value itself is never returned.
	CourtScorePasscode bool `json:"courtScorePasscode"`
	// RequiresRegistrationCode reports whether the event has an invite code set,
	// so the public form knows to ask for one. The code VALUE itself is never
	// returned (write-only secret, like AdminPasscode).
	RequiresRegistrationCode bool `json:"requiresRegistrationCode"`
	// RegistrationCloseAt (RFC3339) is an explicit self-registration cutoff; nil =
	// no cutoff (falls back to the event-day close). Enforced on self-registration.
	RegistrationCloseAt *string `json:"registrationCloseAt,omitempty"`
	// RoundStartMinutes is the organizer's proposed start time (minute-of-day) per
	// round number, e.g. {"1":540,"2":600}. The client schedule cascade anchors
	// each round to its proposed time; empty = auto-pack. No wall-clock is stored.
	RoundStartMinutes map[string]int `json:"roundStartMinutes,omitempty"`
	// PosterURL is the uploaded event poster (public Storage URL), or nil.
	PosterURL *string `json:"posterUrl,omitempty"`
	// Recurring social: RecurIntervalDays>0 marks a series head that auto-spawns
	// occurrences every N days; RecurUntil caps the run. SeriesID links every
	// occurrence (and the head) of one recurring social.
	RecurIntervalDays int     `json:"recurIntervalDays,omitempty"`
	RecurUntil        *string `json:"recurUntil,omitempty"`
	SeriesID          *string `json:"seriesId,omitempty"`
	// Perpetual marks the single ongoing event of a recurring/"forever" league —
	// it does NOT clone weekly (recur_interval_days is 0); instead standings/games
	// accumulate season-long and check-ins auto-reset each day. Set on adoption.
	Perpetual bool `json:"perpetual,omitempty"`
	// RecurPaused pauses a perpetual league (no play until resumed). RecurSkipUntil
	// skips sessions up to and including that date (YYYY-MM-DD). Both drive the
	// header/feed; the weekday+time come from StartsAt (reschedule updates it).
	RecurPaused    bool    `json:"recurPaused,omitempty"`
	RecurSkipUntil *string `json:"recurSkipUntil,omitempty"`
	// SeasonNumber / SeasonStartedAt scope a perpetual league's live standings to
	// the CURRENT season. SeasonStartedAt (ISO, empty = all-time/season 1) is the
	// cutoff: only rounds created on/after it count. "Start new season" archives
	// the standings and bumps these. See season.go.
	SeasonNumber    int    `json:"seasonNumber,omitempty"`
	SeasonStartedAt string `json:"seasonStartedAt,omitempty"`
	// CoachLed is true when this event belongs to a coach-led league (its players
	// auto-enroll as the league coach's students). Resolved on GetEvent from the
	// league; drives a "Coach-led" badge in the header. Not a stored event column.
	CoachLed bool `json:"coachLed,omitempty"`
	// RotationLadder narrows LadderLeague: true only for the live "up and down
	// the river" format. Both formats share the event shell, but they are
	// different games — the client names the tab and picks the panel from this.
	RotationLadder bool `json:"rotationLadder,omitempty"`
	// DistanceKm is set only in Nearby results — km from the requester.
	DistanceKm *float64 `json:"distanceKm,omitempty"`
	// ScheduleBreaks are organizer-defined blocked time ranges (e.g. lunch) the
	// schedule timeline skips. Minutes from midnight; applied to each day.
	ScheduleBreaks []ScheduleBreak `json:"scheduleBreaks"`
	// DayCapMinutes: if set, no games start past this time-of-day; the rest roll
	// to the next tournament day. Minutes from midnight; nil = no cap.
	DayCapMinutes *int `json:"dayCapMinutes,omitempty"`
	// DayEndMinutes: per-day court closing time (minutes from midnight), indexed
	// by tournament day. A game that wouldn't FINISH before its day's close rolls
	// to the next day (last/only day → flagged as running past close). -1 or a
	// missing slot = no close that day. Takes precedence over DayCapMinutes.
	DayEndMinutes []int `json:"dayEndMinutes,omitempty"`
	// LeagueID links this event to a league (season/recurring play) it belongs to,
	// or nil for a standalone event.
	LeagueID *string `json:"leagueId,omitempty"`
	// LadderLeague marks an event that IS a challenge ladder's ongoing event, so
	// the app can show the ladder itself (rungs, challenges, history) alongside
	// the usual event tabs. Resolved from the league on GetEvent.
	LadderLeague bool `json:"ladderLeague,omitempty"`
	// Sponsor watermark — a low-opacity sponsor logo/mascot rendered BEHIND the
	// event's surfaces. URL "" = none; opacity 0–1; scale 0.1–1; position one of
	// center|top-left|top-right|bottom-left|bottom-right|tiled.
	SponsorWatermarkURL      string  `json:"sponsorWatermarkUrl,omitempty"`
	SponsorWatermarkOpacity  float64 `json:"sponsorWatermarkOpacity"`
	SponsorWatermarkPosition string  `json:"sponsorWatermarkPosition,omitempty"`
	SponsorWatermarkScale    float64 `json:"sponsorWatermarkScale"`
	// OwnerID is the event owner's user id, so the app can gate the organizer
	// dashboard to a read-only view for non-owners regardless of how the event
	// screen was opened (club events, notifications, deep links).
	OwnerID string `json:"ownerId,omitempty"`
	// OwnerPremium = the event owner has an active Premium plan. Set on single
	// reads (GetEvent) so public views can hide the free-tier house-brand mark.
	OwnerPremium bool `json:"ownerPremium"`
	// ViewerRegistered = the authenticated caller is an approved player of this
	// event. Set on single reads (GetEvent, optionalAuth) so the client can show
	// the feed composer to registered players (not to random spectators).
	ViewerRegistered bool `json:"viewerRegistered,omitempty"`
	// OrganizerName is the event owner's display name (from pmp_profiles), set on
	// single reads (GetEvent) for the tournament-info tab. Empty when unknown.
	OrganizerName string `json:"organizerName,omitempty"`
	// ScoreboardTheme is the per-event live-board look: { bg, text, accent, font }
	// (hex color strings + a font-family key). Nil = the default house theme.
	// Ships in add_scoreboard_theme.sql.
	ScoreboardTheme map[string]any `json:"scoreboardTheme,omitempty"`
}

// PublicEvent is the SAFE, public-facing projection of an Event served at
// GET /events/public (the planmypickle.com marketing feed). It deliberately
// omits every private field — no owner_id, passcode, registrant PII, finance,
// or contact phone — so it can be read with no auth and from any origin.
type PublicEvent struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	TournamentFormat string  `json:"tournamentFormat"` // round_robin | single_elim | pools_playoff
	Format           string  `json:"format"`           // singles | doubles
	StartsAt         *string `json:"startsAt,omitempty"`
	EndsAt           *string `json:"endsAt,omitempty"`
	Location         *string `json:"location,omitempty"`
	VenueName        *string `json:"venueName,omitempty"`
	PosterURL        *string `json:"posterUrl,omitempty"`
	DuprSanctioned   bool    `json:"duprSanctioned"`
	RegisteredCount  int     `json:"registeredCount"`
	// CreatedAt lets the "newly added" home rail sort/label by recency.
	CreatedAt string `json:"createdAt,omitempty"`
	// City + County + State power metro/programmatic-SEO directory pages
	// (filterable via ?county= on the public feed). City is what people search;
	// county is the fallback when the geocoder couldn't name a municipality.
	City   string `json:"city,omitempty"`
	County string `json:"county,omitempty"`
	State  string `json:"state,omitempty"`
}

// PublicLeague is a listed league projected for the public SEO hubs. County/State
// are derived from the league's events (sessions), not stored on the league.
type PublicLeague struct {
	ID           string
	Name         string
	LeagueType   string
	Sanctioned   bool
	Description  string
	County       string
	State        string
	SessionCount int
	NextDate     string // earliest session starts_at (RFC3339), "" if none
}

// League groups multiple EXISTING events (each event = a session) for recurring
// or season play; standings aggregate every player's record across all of them.
// Owner-scoped (OwnerID = the organizer's auth user id), like events.
type League struct {
	ID          string  `json:"id"`
	OwnerID     string  `json:"ownerId"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
	CreatedAt   string  `json:"createdAt"`
	// PosterURL is the uploaded league banner (public Storage URL), or nil.
	PosterURL *string `json:"posterUrl,omitempty"`
	// LeagueType: round_robin | ladder | team. DayType: single | multi.
	LeagueType string `json:"leagueType"`
	DayType    string `json:"dayType"`
	// LadderLoserMode (rotation ladders): 'down' = losers drop a court (the
	// classic river); 'stay' = losers hold their court and only winners climb,
	// with the TOP court's losers falling to the bottom. Fixed at creation.
	LadderLoserMode string `json:"ladderLoserMode,omitempty"`
	// LadderFormat (ladder leagues only): challenge | rotation. 'challenge' is the
	// persistent challenge ladder; 'rotation' is the live "up & down the river"
	// session league. Empty/other for non-ladder leagues.
	LadderFormat string `json:"ladderFormat,omitempty"`
	// Sanctioned flags an officially sanctioned league.
	Sanctioned bool `json:"sanctioned"`
	// Listed opts the league into public discovery (the "pickleball leagues in
	// <city>" SEO hubs). Default false = private. City/state is derived from its
	// events, not stored on the league.
	Listed bool `json:"listed"`
	// CashPrize flags a cash-prize league; CashPrizeAmount is the optional pot.
	CashPrize       bool     `json:"cashPrize"`
	CashPrizeAmount *float64 `json:"cashPrizeAmount,omitempty"`
	// FirstSessionAt / LastSessionAt are the earliest start and latest end (or
	// start) across the league's sessions (events), RFC3339 UTC — populated on
	// the MyLeagues list so the home screen can group leagues by lifecycle
	// (Happening now / Upcoming / Past). Nil when the league has no dated session.
	FirstSessionAt *string `json:"firstSessionAt,omitempty"`
	LastSessionAt  *string `json:"lastSessionAt,omitempty"`
	// Ladder is the rule config for ladder-type leagues (nil for others).
	Ladder *LadderConfig `json:"ladder,omitempty"`
	// Location is a free-text venue/place for the league (esp. ladders).
	Location *string `json:"location,omitempty"`
	// CoachLed marks a league whose owner (a coach) auto-enrolls every registrant
	// as a coaching student, so they can give per-player feedback from the Coach
	// tab. CoachID is that coach (the league owner). CoachLed=false when off.
	CoachLed bool    `json:"coachLed"`
	CoachID  *string `json:"coachId,omitempty"`
	// CourtCount is the default number of courts the league's sessions schedule
	// on (a "New session" seeds its num_courts from this). Nil = per-session.
	CourtCount *int `json:"courtCount,omitempty"`
	// WinBy is the default win margin (1 or 2) for the sessions this league
	// creates. Nil = the app-wide default of 2. A session COPIES it at creation
	// and owns its value from then on, so changing this never restates a game
	// that has already been played.
	WinBy *int `json:"winBy,omitempty"`
	// Recurs marks a league on a recurring schedule (a weekly RR session auto-
	// spawns forever). RecurStartAt is the anchor (RFC3339 UTC) — the weekday +
	// time are derived from it. Nil/false when there's no recurring schedule.
	Recurs       bool    `json:"recurs"`
	RecurStartAt *string `json:"recurStartAt,omitempty"`
	// OngoingEventID is the single perpetual event a recurring/"forever" league
	// runs as — the one ongoing tournament (Feed/Players/Game/Standings…). Set
	// once the league is adopted into the single-event model; the client opens
	// this event directly instead of a per-session list. Nil for session-based
	// leagues.
	OngoingEventID *string `json:"ongoingEventId,omitempty"`
}

// LeagueMember is someone who joined a league once and is auto-rostered into
// every session. Added by name + email/phone, invited by text/email, claimed by
// token → UserID set (mirrors CoachStudent).
type LeagueMember struct {
	ID        string `json:"id"`
	LeagueID  string `json:"leagueId"`
	UserID    string `json:"userId,omitempty"`
	FullName  string `json:"fullName"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Linked    bool   `json:"linked"` // has a resolved account
	CreatedAt string `json:"createdAt"`
}

// LeagueVideo is a clip a league member posted to the league's video feed.
type LeagueVideo struct {
	ID           string `json:"id"`
	LeagueID     string `json:"leagueId"`
	UploadedBy   string `json:"uploadedBy"`
	UploaderName string `json:"uploaderName"`
	VideoURL     string `json:"videoUrl"`
	Title        string `json:"title,omitempty"`
	CreatedAt    string `json:"createdAt"`
}

// CreateLeagueRequest is the create-payload for a league.
type CreateLeagueRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	LeagueType  string `json:"leagueType"` // round_robin | ladder | team (default round_robin)
	DayType     string `json:"dayType"`    // single | multi (default multi)
	// LadderFormat (ladder only): challenge (default) | rotation.
	LadderFormat string `json:"ladderFormat"`
	// LadderLoserMode (rotation ladders): 'down' (default) | 'stay'. Fixed at
	// creation — the room has to know the rule before the first ball is served.
	LadderLoserMode string   `json:"ladderLoserMode,omitempty"`
	Sanctioned      bool     `json:"sanctioned"`
	Listed          bool     `json:"listed"` // opt into public discovery (default false)
	CashPrize       bool     `json:"cashPrize"`
	CashPrizeAmount *float64 `json:"cashPrizeAmount,omitempty"`
	// Ladder config (optional; only applied when LeagueType == "ladder"). Nil
	// leaves the schema defaults (leapfrog / unlimited / no inactivity).
	Ladder *LadderConfig `json:"ladder,omitempty"`
	// Location is a free-text venue/place for the league (optional).
	Location string `json:"location"`
	// CoachLed makes this a coach-led league (instructor owners only): every
	// registrant is auto-enrolled as the owner's coaching student.
	CoachLed bool `json:"coachLed"`
	// CourtCount is the default court count the league's sessions schedule on.
	CourtCount *int `json:"courtCount,omitempty"`
	// WinBy is the default win margin (1 or 2) for this league's sessions.
	// Nil leaves the app-wide default of 2.
	WinBy *int `json:"winBy,omitempty"`
	// Divisions are the league's brackets (skill/age/DUPR bands). Empty creates
	// a single "Open" division by default (mirrors event creation).
	Divisions []LeagueBracketInput `json:"divisions"`
}

// LeagueBracketInput is one division to create under a league (mirrors
// BracketInput but for a league). DivisionType defaults to "open" when empty.
type LeagueBracketInput struct {
	Name         string   `json:"name"`
	DivisionType string   `json:"divisionType"` // default "open" (see LeagueBracket)
	MinRating    *float64 `json:"minRating,omitempty"`
	MaxRating    *float64 `json:"maxRating,omitempty"`
	MinAge       *int     `json:"minAge,omitempty"`
	MaxAge       *int     `json:"maxAge,omitempty"`
	DuprMin      *float64 `json:"duprMin,omitempty"`
	DuprMax      *float64 `json:"duprMax,omitempty"`
	SortOrder    int      `json:"sortOrder"`
}

// AddEventToLeagueRequest links an existing (caller-owned) event into a league.
type AddEventToLeagueRequest struct {
	EventID string `json:"eventId"`
}

// LeagueDetail is a league plus its sessions (events, ordered by start date)
// and its divisions (brackets, ordered by sort_order).
type LeagueDetail struct {
	League
	Events   []Event         `json:"events"`
	Brackets []LeagueBracket `json:"brackets"`
}

// ScheduleBreak is a blocked time range (minutes from midnight) the schedule
// timeline skips over — e.g. a lunch break.
type ScheduleBreak struct {
	StartMin int    `json:"startMin"`
	EndMin   int    `json:"endMin"`
	Label    string `json:"label"`
}

type Bracket struct {
	ID        string   `json:"id"`
	EventID   string   `json:"eventId"`
	Name      string   `json:"name"`
	MinRating *float64 `json:"minRating,omitempty"`
	MaxRating *float64 `json:"maxRating,omitempty"`
	MinAge    *int     `json:"minAge,omitempty"`
	MaxAge    *int     `json:"maxAge,omitempty"`
	SortOrder int      `json:"sortOrder"`
	// DivisionType: open | mens_doubles | womens_doubles | mixed_doubles |
	// singles | team_play (defaults to "open").
	DivisionType string `json:"divisionType"`
	// DuprMin/DuprMax are the optional DUPR rating band (distinct from the
	// self-rated skill band in MinRating/MaxRating).
	DuprMin *float64 `json:"duprMin,omitempty"`
	DuprMax *float64 `json:"duprMax,omitempty"`
	// Courts pins this division to a specific set of court numbers when the
	// scheduler arranges games; empty/nil = the division may use any court.
	Courts []int `json:"courts,omitempty"`
	// PlayerCount is the expected number of players in this division — used to
	// fill it with that many placeholder players for a pre-registration schedule
	// preview. nil = no target.
	PlayerCount *int `json:"playerCount,omitempty"`
	// StartMinutes is this division's wave start time as minute-of-day (e.g. 480
	// = 8:00am). Lets rating waves stagger (2.5 @ 8am, 3.0 @ 11am, …). nil = starts
	// with the event.
	StartMinutes *int `json:"startMinutes,omitempty"`
	// TeamCount / PlayersPerTeam configure team (MLP) divisions: how many teams to
	// build and how many players each carries. nil = format default.
	TeamCount      *int `json:"teamCount,omitempty"`
	PlayersPerTeam *int `json:"playersPerTeam,omitempty"`
}

// LeagueBracket is a division within a league, mirroring Bracket but keyed on a
// league instead of an event.
type LeagueBracket struct {
	ID           string   `json:"id"`
	LeagueID     string   `json:"leagueId"`
	Name         string   `json:"name"`
	DivisionType string   `json:"divisionType"`
	MinRating    *float64 `json:"minRating,omitempty"`
	MaxRating    *float64 `json:"maxRating,omitempty"`
	MinAge       *int     `json:"minAge,omitempty"`
	MaxAge       *int     `json:"maxAge,omitempty"`
	DuprMin      *float64 `json:"duprMin,omitempty"`
	DuprMax      *float64 `json:"duprMax,omitempty"`
	SortOrder    int      `json:"sortOrder"`
	// EntrantCount is the number of ladder entrants in this division (populated
	// on the league-detail read for ladder leagues; 0 otherwise).
	EntrantCount int `json:"entrantCount"`
}

// LadderEntrant is one competitor on a league division's ladder. Position is the
// 1-based rank (1 = top of the ladder). PlayerID optionally links to a real app
// player; otherwise the entrant is just a free-text display name the organizer
// typed. Ladder leagues (leagues.league_type == 'ladder') are organizer-driven —
// player self-service challenges are an explicit FUTURE v2.
type LadderEntrant struct {
	ID              string  `json:"id"`
	LeagueBracketID string  `json:"leagueBracketId"`
	DisplayName     string  `json:"displayName"`
	PlayerID        *string `json:"playerId,omitempty"`
	IsTeam          bool    `json:"isTeam"`
	Position        int     `json:"position"`
	// Win/loss/tie record, tallied from the division's recorded matches.
	Wins   int `json:"wins"`
	Losses int `json:"losses"`
	Ties   int `json:"ties"`
	// LastActiveAt is the entrant's most recent match time (RFC3339), "" if none —
	// powers the inactivity policy + a "last played" display.
	LastActiveAt string `json:"lastActiveAt,omitempty"`
}

// LadderMatch is one recorded result between two entrants on a division's ladder
// (the immutable history). WinnerEntrantID is whichever of A/B won; Score is
// free-form ("11-7" / "11-9, 7-11, 11-5") so 1-day, multi-day and best-of-N all
// fit. The leapfrog reorder (applied when a lower-ranked entrant wins) is a
// side-effect of recording the match, not stored on this row.
type LadderMatch struct {
	ID              string `json:"id"`
	LeagueBracketID string `json:"leagueBracketId"`
	EntrantAID      string `json:"entrantAId"`
	EntrantBID      string `json:"entrantBId"`
	WinnerEntrantID string `json:"winnerEntrantId"`
	Score           string `json:"score,omitempty"`
	PlayedAt        string `json:"playedAt"`
}

// AddLadderEntrantRequest adds an entrant to a division's ladder. A new entrant
// joins at the BOTTOM (the service computes its position). PlayerID is optional.
type AddLadderEntrantRequest struct {
	DisplayName string  `json:"displayName"`
	PlayerID    *string `json:"playerId,omitempty"`
	IsTeam      bool    `json:"isTeam"`
}

// RecordLadderResultRequest records a match between two entrants and applies the
// leapfrog reorder. WinnerEntrantID must be one of A/B. Score is optional.
type RecordLadderResultRequest struct {
	EntrantAID      string `json:"entrantAId"`
	EntrantBID      string `json:"entrantBId"`
	WinnerEntrantID string `json:"winnerEntrantId"`
	// Tie records a drawn match (no winner) — positions stay unchanged. An empty
	// WinnerEntrantID is also treated as a tie.
	Tie   bool   `json:"tie"`
	Score string `json:"score"`
	// PlayedAt is an optional ISO-8601 timestamp; empty defaults to now().
	PlayedAt string `json:"playedAt"`
}

// MoveLadderEntrantRequest repositions an entrant to a new 1-based rank
// (organizer reorder / reseed).
type MoveLadderEntrantRequest struct {
	NewPosition int `json:"newPosition"`
}

// --- Rotation session ("up and down the river" / king-of-the-court) ---------
//
// A LIVE, timed session that runs UNDER a ladder division: players self-rate and
// are seeded onto numbered courts (1 = top), play timed rounds, and each round's
// winners move up a court / losers down (engine.NextRound). Cumulative game-wins
// are the standings. Distinct from the persistent challenge ladder above.

// RotationSession is one session + its round-timer state.
type RotationSession struct {
	ID              string `json:"id"`
	LeagueBracketID string `json:"leagueBracketId"`
	Name            string `json:"name"`
	Status          string `json:"status"` // setup | live | paused | done
	CourtCount      int    `json:"courtCount"`
	RoundMinutes    int    `json:"roundMinutes"`
	// AutoAdvance: app rotates automatically at the buzzer (true, default) vs the
	// organizer taps "Next round" (false).
	AutoAdvance  bool `json:"autoAdvance"`
	CurrentRound int  `json:"currentRound"`
	// RoundStartedAt / RoundEndsAt (RFC3339, "" when not live) drive the countdown
	// every client renders; RoundEndsAt is the buzzer time.
	RoundStartedAt string `json:"roundStartedAt,omitempty"`
	RoundEndsAt    string `json:"roundEndsAt,omitempty"`
	// PausedAt is when the clock was stopped ("" while running). RoundEndsAt is
	// deliberately left untouched by a pause so the remaining time survives, so
	// a paused countdown must be read as RoundEndsAt - PausedAt; against "now"
	// it would keep falling while nobody is playing.
	PausedAt  string `json:"pausedAt,omitempty"`
	CreatedAt string `json:"createdAt"`
	// PlannedRounds stops the night after N rounds (0 = run until the organizer
	// ends it, which is the original behaviour). StopAt refuses to START a round
	// that would run past that time — the courts are booked until then.
	//
	// Neither ever cuts a round short: whatever is being played finishes.
	PlannedRounds int    `json:"plannedRounds,omitempty"`
	StopAt        string `json:"stopAt,omitempty"`
	// LoserMode ('down' | 'stay') is the league's loser rule, carried on the
	// board so every player can see which of the two games is being played
	// without opening the league.
	LoserMode string `json:"loserMode,omitempty"`
}

// SubstituteRotationPlayerRequest hands OutPlayerID's seat to a new roster row.
// The outgoing player keeps the score they have; the substitute starts fresh.
type SubstituteRotationPlayerRequest struct {
	OutPlayerID string  `json:"outPlayerId"`
	DisplayName string  `json:"displayName"`
	EntrantID   *string `json:"entrantId,omitempty"`
	// SelfRating out of 1..7 inherits the outgoing player's rating.
	SelfRating float64 `json:"selfRating"`
}

// RotationSubstitution is one swap: who went out, who came in, and the round it
// took effect (the boundary between the two records).
type RotationSubstitution struct {
	Round       int    `json:"round"`
	OutPlayerID string `json:"outPlayerId"`
	InPlayerID  string `json:"inPlayerId"`
}

// RotationPlayer is one competitor in a session's roster snapshot, with the
// self-rating that seeds them and the win/game tallies that rank the standings.
type RotationPlayer struct {
	ID          string  `json:"id"`
	SessionID   string  `json:"sessionId"`
	EntrantID   *string `json:"entrantId,omitempty"`
	DisplayName string  `json:"displayName"`
	SelfRating  float64 `json:"selfRating"`
	Wins        int     `json:"wins"`
	Games       int     `json:"games"`
	Active      bool    `json:"active"`
	// StartCourt is the court this player was placed on for round 1, by hand or
	// by shuffle. nil = unplaced, seeded by rating like it always was.
	StartCourt *int `json:"startCourt,omitempty"`
	// Points is the player's TOTAL across every round of the scorecard (the sum
	// of RotationScorecard cells). 0 when the scorecard isn't in use.
	Points int `json:"points"`
}

// RotationScorecard is the organizer's grid: players down the side, rounds
// across the top. Scores[playerID][round] holds the entered value; a missing
// entry means that cell is still blank.
type RotationScorecard struct {
	// Rounds played so far (1..currentRound), for the column headers.
	Rounds []int `json:"rounds"`
	// playerID -> round -> score.
	Scores map[string]map[int]int `json:"scores"`
	// Available means the scorecard TABLE exists (migration run). Enabled means
	// THIS session is actually using it (it has scorecard rows). The split keeps
	// existing sessions on their original who-won flow instead of flipping every
	// session into scorecard mode the moment the migration lands.
	Available bool `json:"available"`
	Enabled   bool `json:"enabled"`
	// Played maps a round to the players who were ON COURT for it, so the grid
	// can mark everyone else that round as resting — including in rounds already
	// finished. Without it only the current round is knowable client-side, and
	// scrolling back a column offered a score cell for a night someone spent on
	// the bench. Absent for rounds whose layout doesn't exist yet.
	Played map[int][]string `json:"played,omitempty"`
}

// RotationCourt is one court in one round: the four players as two teams (a/b),
// court 1 = top, plus the reported winner ("a" | "b" | ""). Player fields carry
// display names (resolved from the roster) so the live board renders directly.
type RotationCourt struct {
	Court  int                 `json:"court"`
	Round  int                 `json:"round"`
	TeamA  []RotationCourtSeat `json:"teamA"`
	TeamB  []RotationCourtSeat `json:"teamB"`
	Winner string              `json:"winner,omitempty"`
}

// RotationCourtSeat is one player's slot on a court (id + display name).
type RotationCourtSeat struct {
	PlayerID    string `json:"playerId"`
	DisplayName string `json:"displayName"`
}

// RotationBoard is the full live view: the session, its roster (standings order),
// and the current round's courts. It's what both the organizer board and each
// player's screen render from.
type RotationBoard struct {
	Session   RotationSession  `json:"session"`
	Players   []RotationPlayer `json:"players"`
	Courts    []RotationCourt  `json:"courts"`
	Standings []RotationPlayer `json:"standings"`
	// Byes are the players sitting out the CURRENT round (the bench), in rotation
	// order — empty when everyone plays (roster ≤ courts×4).
	Byes []RotationPlayer `json:"byes"`
	// Scorecard is the organizer's name × round grid (the primary way results are
	// recorded — the court winner is derived from these scores).
	Scorecard RotationScorecard `json:"scorecard"`
	// Substitutions, oldest first — who took over from whom, and at which round.
	// Empty for the ordinary session where nobody left early.
	Substitutions []RotationSubstitution `json:"substitutions"`
}

// CreateRotationSessionRequest opens a new session under a ladder division.
type CreateRotationSessionRequest struct {
	Name         string `json:"name"`
	CourtCount   int    `json:"courtCount"`
	RoundMinutes int    `json:"roundMinutes"`
	// AutoAdvance defaults to true (nil-safe via a pointer so an omitted field
	// still means "auto").
	AutoAdvance *bool `json:"autoAdvance,omitempty"`
}

// AddRotationPlayerRequest adds a competitor to a session's roster. EntrantID
// optionally links a ladder entrant (else a walk-up with just a name).
type AddRotationPlayerRequest struct {
	DisplayName string  `json:"displayName"`
	EntrantID   *string `json:"entrantId,omitempty"`
	SelfRating  float64 `json:"selfRating"`
}

// ReportRotationCourtRequest reports which team won a court in the current round.
type ReportRotationCourtRequest struct {
	Round  int    `json:"round"`
	Court  int    `json:"court"`
	Winner string `json:"winner"` // "a" | "b"
}

// LadderConfig is a ladder league's rule configuration (stored on the league).
// Defaults reproduce the classic behavior: leapfrog reorder, unlimited challenge
// range, no inactivity penalty. Response/play/inactivity knobs drive the phase-2
// player-driven challenge lifecycle.
type LadderConfig struct {
	ReorderModel     string `json:"reorderModel"`     // leapfrog | swap
	ChallengeRange   int    `json:"challengeRange"`   // 0 = unlimited; N = spots above
	ResponseDays     int    `json:"responseDays"`     // days to accept a challenge
	PlayDays         int    `json:"playDays"`         // days to play once accepted
	InactivityDays   int    `json:"inactivityDays"`   // 0 = off; N idle days → action
	InactivityAction string `json:"inactivityAction"` // none | drop_one | drop_bottom
}

// LadderChallenge is one player-driven challenge between two entrants on a
// division's ladder, with its lifecycle status and deadline timers.
type LadderChallenge struct {
	ID                  string  `json:"id"`
	LeagueBracketID     string  `json:"leagueBracketId"`
	ChallengerEntrantID string  `json:"challengerEntrantId"`
	ChallengedEntrantID string  `json:"challengedEntrantId"`
	ChallengerName      string  `json:"challengerName"`
	ChallengedName      string  `json:"challengedName"`
	Status              string  `json:"status"` // pending|accepted|completed|forfeited|declined|voided|cancelled
	RespondBy           string  `json:"respondBy,omitempty"`
	PlayBy              string  `json:"playBy,omitempty"`
	ResultMatchID       *string `json:"resultMatchId,omitempty"`
	CreatedAt           string  `json:"createdAt"`
	ResolvedAt          string  `json:"resolvedAt,omitempty"`
	// Viewer-relative helpers (set on the /me list): is the caller the challenger?
	Mine         bool `json:"mine,omitempty"`
	IsChallenger bool `json:"isChallenger,omitempty"`
}

// IssueChallengeRequest challenges an entrant above the caller on a division.
type IssueChallengeRequest struct {
	ChallengedEntrantID string `json:"challengedEntrantId"`
}

// ReportChallengeRequest reports a challenge's played result. WinnerSide is
// 'challenger' | 'challenged' | 'tie' — never a raw entrant id (the backend maps
// it against the challenge row, closing the entrant-id IDOR).
type ReportChallengeRequest struct {
	WinnerSide string `json:"winnerSide"`
	Score      string `json:"score"`
}

// Team is one team on a league division (Team League — the SIMPLE single-fixture
// model). Name is the display name; PlayerID optionally links to a real app
// player (e.g. the captain) — the roster is minimal and NOT required to score.
// Team leagues (leagues.league_type == 'team') are organizer-driven.
type Team struct {
	ID              string  `json:"id"`
	LeagueBracketID string  `json:"leagueBracketId"`
	Name            string  `json:"name"`
	PlayerID        *string `json:"playerId,omitempty"`
}

// TeamFixture is one recorded result between two teams on a division (the
// immutable history). WinnerTeamID is whichever of A/B won; Score is free-form
// ("3-1" games won, or "11-7, 9-11, 11-5") — the single-fixture model keeps it
// as one optional string with NO per-line detail.
type TeamFixture struct {
	ID              string `json:"id"`
	LeagueBracketID string `json:"leagueBracketId"`
	TeamAID         string `json:"teamAId"`
	TeamBID         string `json:"teamBId"`
	WinnerTeamID    string `json:"winnerTeamId"`
	Score           string `json:"score,omitempty"`
	PlayedAt        string `json:"playedAt"`
}

// TeamStanding is a team's computed record on a division: fixtures won/lost and
// win %. NOT stored — computed in Go from the recorded fixtures (no leapfrog),
// ordered by wins then win %.
type TeamStanding struct {
	TeamID string `json:"teamId"`
	Name   string `json:"name"`
	Wins   int    `json:"wins"`
	Losses int    `json:"losses"`
	Played int    `json:"played"`
	// WinPct is wins / played in [0,1]; 0 when the team has no fixtures.
	WinPct float64 `json:"winPct"`
}

// AddTeamRequest adds a team to a division. PlayerID is optional (roster link).
type AddTeamRequest struct {
	Name     string  `json:"name"`
	PlayerID *string `json:"playerId,omitempty"`
}

// RecordFixtureRequest records a fixture between two teams. WinnerTeamID must be
// one of A/B. Score is optional free-text.
type RecordFixtureRequest struct {
	TeamAID      string `json:"teamAId"`
	TeamBID      string `json:"teamBId"`
	WinnerTeamID string `json:"winnerTeamId"`
	Score        string `json:"score"`
	// PlayedAt is an optional ISO-8601 timestamp; empty defaults to now().
	PlayedAt string `json:"playedAt"`
}

// ============================================================================
// MLP-style team events. A team-format event (events.team_size > 0) registers
// TEAMS of players (each with a gender); the existing tournament_format brackets
// the teams. Each team-vs-team matchup is a TeamTie whose lines REUSE the
// matches table (matches.tie_id + line_type): women's (wd), men's (md), two
// mixed (mx1, mx2), plus a lazily-created decider (dec) when the lines split 2-2.
// ============================================================================

// EventTeam is a team in a team-format event, optionally scoped to a pool.
type EventTeam struct {
	ID        string  `json:"id"`
	EventID   string  `json:"eventId"`
	BracketID *string `json:"bracketId,omitempty"`
	Name      string  `json:"name"`
	Seed      *int    `json:"seed,omitempty"`
	// BannerURL is the team's custom banner (Manage Teams upload) — shown on
	// tie cards in Live scoring, Standings, and the Live TV board.
	BannerURL string       `json:"bannerUrl,omitempty"`
	Members   []TeamMember `json:"members,omitempty"`
}

// TeamMember is a roster member; Gender (M|F) drives line eligibility.
type TeamMember struct {
	ID         string   `json:"id"`
	TeamID     string   `json:"teamId"`
	PlayerID   *string  `json:"playerId,omitempty"`
	FullName   string   `json:"fullName"`
	Gender     string   `json:"gender"` // M | F
	CheckedIn  bool     `json:"checkedIn"`
	Phone      string   `json:"phone,omitempty"`
	DuprID     string   `json:"duprId,omitempty"`
	DuprRating *float64 `json:"duprRating,omitempty"`
}

// TeamTie is a team-vs-team matchup; its lines live in matches (tie_id).
// WinnerTeamID is rolled up from lines won (2-2 broken by the decider line).
type TeamTie struct {
	ID           string    `json:"id"`
	EventID      string    `json:"eventId"`
	BracketID    *string   `json:"bracketId,omitempty"`
	Stage        string    `json:"stage"`
	TeamAID      string    `json:"teamAId"`
	TeamBID      string    `json:"teamBId"`
	WinnerTeamID *string   `json:"winnerTeamId,omitempty"`
	Status       string    `json:"status"`
	Round        int       `json:"round,omitempty"` // playoff round (1=first); 0 for pool
	Lines        []TieLine `json:"lines,omitempty"`
}

// TieLine is one line of a tie (a matches row) with its per-line result + the
// player ids on each side (team A = 1, team B = 2). WinningTeam is 1 | 2 | 0.
type TieLine struct {
	MatchID      string   `json:"matchId"`
	LineType     string   `json:"lineType"` // wd | md | mx1 | mx2 | dec
	Status       string   `json:"status"`
	Team1Score   *int     `json:"team1Score,omitempty"`
	Team2Score   *int     `json:"team2Score,omitempty"`
	WinningTeam  int      `json:"winningTeam"`
	Team1Players []string `json:"team1Players"`
	Team2Players []string `json:"team2Players"`
}

// SetLineupRequest assigns the players for one tie line (each side from its own
// team's roster; gender + count must match the line type).
type SetLineupRequest struct {
	Team1 []string `json:"team1"`
	Team2 []string `json:"team2"`
}

// TeamEventStanding is a team's record in a team event, ordered by ties won,
// then lines won, then point differential.
type TeamEventStanding struct {
	TeamID        string `json:"teamId"`
	Name          string `json:"name"`
	BannerURL     string `json:"bannerUrl,omitempty"`
	TiesWon       int    `json:"tiesWon"`
	TiesLost      int    `json:"tiesLost"`
	LinesWon      int    `json:"linesWon"`
	LinesLost     int    `json:"linesLost"`
	PointsFor     int    `json:"pointsFor"`
	PointsAgainst int    `json:"pointsAgainst"`
}

// CreateTeamRequest registers a team on a team-format event.
type CreateTeamRequest struct {
	Name      string  `json:"name"`
	BracketID *string `json:"bracketId,omitempty"`
}

// AddTeamMemberRequest adds a roster member (gender required for line eligibility).
type AddTeamMemberRequest struct {
	FullName   string   `json:"fullName"`
	Gender     string   `json:"gender"` // M | F
	PlayerID   *string  `json:"playerId,omitempty"`
	Phone      string   `json:"phone,omitempty"`
	DuprID     string   `json:"duprId,omitempty"`
	DuprRating *float64 `json:"duprRating,omitempty"`
}

// FlexMatchup is one team-pair matchup in a Flex league division's generated
// round-robin schedule (Flex League — the self-scheduled season). It reuses the
// `teams` table for entrants. A matchup starts pending (generated, not yet
// played); recording a result sets WinnerTeamID (one of A/B), an optional
// free-text Score, PlayedAt, and flips Status to "completed". Standings are
// computed in Go from the COMPLETED matchups (reusing the Team-league math).
type FlexMatchup struct {
	ID              string `json:"id"`
	LeagueBracketID string `json:"leagueBracketId"`
	TeamAID         string `json:"teamAId"`
	TeamBID         string `json:"teamBId"`
	// WinnerTeamID is whichever of A/B won, or "" while the matchup is pending.
	WinnerTeamID string `json:"winnerTeamId,omitempty"`
	Score        string `json:"score,omitempty"`
	// Status: pending | completed.
	Status string `json:"status"`
	// PlayedAt is set only once the matchup is completed; "" while pending.
	PlayedAt string `json:"playedAt,omitempty"`
}

// RecordFlexResultRequest records the result of a pending Flex matchup, flipping
// it to completed. WinnerTeamID must be one of the matchup's two teams. Score is
// optional free-text.
type RecordFlexResultRequest struct {
	WinnerTeamID string `json:"winnerTeamId"`
	Score        string `json:"score"`
	// PlayedAt is an optional ISO-8601 timestamp; empty defaults to now().
	PlayedAt string `json:"playedAt"`
}

type Registration struct {
	ID            string  `json:"id"`
	EventID       string  `json:"eventId"`
	PlayerID      string  `json:"playerId"`
	FullName      string  `json:"fullName"`
	BracketID     *string `json:"bracketId,omitempty"`
	PaymentStatus string  `json:"paymentStatus"`
	// Paid add-ons this registrant opted into (charged with their entry fee).
	AddonTee     bool    `json:"addonTee,omitempty"`
	AddonGrips   bool    `json:"addonGrips,omitempty"`
	CheckedIn    bool    `json:"checkedIn"`
	CheckInToken *string `json:"checkInToken,omitempty"`
	// IsSubstitute marks a one-night substitute (benched a member for a session);
	// shown with a "Substitute" badge and expired at the next session build.
	IsSubstitute bool   `json:"isSubstitute,omitempty"`
	Phone        string `json:"phone"`
	// PhotoURL is the registrant's account profile photo (pmp_profiles via the
	// linked user_id), used as their roster avatar; empty for name-only players
	// (the UI falls back to initials).
	PhotoURL string `json:"photoUrl,omitempty"`
	// HasAccount = this registrant's player row is tied to an app account. False
	// means they were added by name/contact only: they cannot see the event, and
	// nothing was ever sent to them. The roster surfaces an Invite action for
	// exactly these rows.
	HasAccount bool     `json:"hasAccount"`
	DuprID     *string  `json:"duprId,omitempty"`
	DuprRating *float64 `json:"duprRating,omitempty"`
	// OutsideRating is true when the player's DUPR rating falls outside their
	// chosen division's rating band. OutsideRatingReason explains it for the
	// organizer's roster (e.g. "DUPR 4.20 is above this division's 3.5 ceiling").
	OutsideRating       bool   `json:"outsideRating"`
	OutsideRatingReason string `json:"outsideRatingReason,omitempty"`
	// Partner pairing (doubles). PartnerID is the partner's PLAYER id when paired
	// with a registered player (set mutually on both registrations); PartnerName
	// is that partner's resolved display name. PartnerNote holds a free-text
	// partner name when the partner isn't a registered player. All nil for an
	// unpaired or singles registration.
	PartnerID   *string `json:"partnerId,omitempty"`
	PartnerName *string `json:"partnerName,omitempty"`
	PartnerNote *string `json:"partnerNote,omitempty"`
	// Approved is false only while a self-registration awaits organizer approval
	// (events.require_approval). Pending entries are kept out of the roster counts
	// and the draw. Defaults true for every pre-existing / organizer-added row.
	Approved bool `json:"approved"`
	// AccountExists is set ONLY on the self-registration response (anonymous):
	// whether an app account already exists for the registrant's email, so the
	// thank-you screen can nudge sign-in vs sign-up. nil otherwise.
	AccountExists *bool `json:"accountExists,omitempty"`
}

type Side struct {
	Team      int      `json:"team"`
	Players   []string `json:"players"`   // display names
	PlayerIDs []string `json:"playerIds"` // parallel to Players — used for swaps
}

// FinanceEntry is a single income or expense line in an event's ledger.
type FinanceEntry struct {
	ID          string `json:"id"`
	EventID     string `json:"eventId"`
	Kind        string `json:"kind"`     // "income" | "expense"
	Category    string `json:"category"` // dropdown "type" value
	AmountCents int    `json:"amountCents"`
	Note        string `json:"note"`
	CreatedAt   string `json:"createdAt"`
}

// FinanceEntryRequest is the create-payload for a ledger line.
type FinanceEntryRequest struct {
	Kind        string `json:"kind"`
	Category    string `json:"category"`
	AmountCents int    `json:"amountCents"`
	Note        string `json:"note"`
}

// ChecklistItem is one tournament-prep to-do (tables, chairs, first aid, …).
type ChecklistItem struct {
	ID        string `json:"id"`
	EventID   string `json:"eventId"`
	Label     string `json:"label"`
	Checked   bool   `json:"checked"`
	SortOrder int    `json:"sortOrder"`
}

// ChecklistItemRequest adds a custom item or updates an item's checked state.
type ChecklistItemRequest struct {
	Label   string `json:"label"`
	Checked bool   `json:"checked"`
}

// Freebie is one giveaway item an organizer stocks + tallies (water, shirts,
// swag). TotalQty is the stock cap (0 = untracked); GivenQty is handed-out.
type Freebie struct {
	ID        string `json:"id"`
	EventID   string `json:"eventId"`
	Name      string `json:"name"`
	TotalQty  int    `json:"totalQty"`
	GivenQty  int    `json:"givenQty"`
	SortOrder int    `json:"sortOrder"`
}

// FreebieRequest creates or edits a freebie (name + stock).
type FreebieRequest struct {
	Name     string `json:"name"`
	TotalQty int    `json:"totalQty"`
}

// FreebieAdjustRequest records a handout (+1) or undoes one (-1).
type FreebieAdjustRequest struct {
	Delta int `json:"delta"`
}

type Match struct {
	ID        string  `json:"id"`
	BracketID *string `json:"bracketId,omitempty"`
	Stage     string  `json:"stage"` // pool | bracket
	// BracketTier classifies a bracket match for rendering: main | consolation |
	// winners | losers | grand_final. Empty/"main" for ordinary brackets.
	BracketTier string `json:"bracketTier,omitempty"`
	// BracketGroup tags a Compass Draw match's direction (east | west | north |
	// south | east_r5 | …) so the UI can split the draw into per-direction
	// brackets. Empty/absent for every non-compass match.
	BracketGroup    string   `json:"bracketGroup,omitempty"`
	BracketRound    *int     `json:"bracketRound,omitempty"`
	BracketSlot     *int     `json:"bracketSlot,omitempty"`
	CourtNumber     *int     `json:"courtNumber,omitempty"`
	PlayOrder       *float64 `json:"playOrder,omitempty"`       // within-court order, lower first
	DurationMinutes *int     `json:"durationMinutes,omitempty"` // per-match length override
	ScheduledDay    *int     `json:"scheduledDay,omitempty"`    // 0-based tournament day; null = auto-split
	Team1Score      *int     `json:"team1Score,omitempty"`      // total points across all games
	Team2Score      *int     `json:"team2Score,omitempty"`
	WinningTeam     *int     `json:"winningTeam,omitempty"` // series winner (games won)
	// LiveTeam1/LiveTeam2 are the RUNNING game score of an in-progress match,
	// pushed point-by-point from the court scorer page for the broadcast overlay.
	// Separate from Team1Score/Team2Score (final, standings-affecting, written
	// only at completion) so live updates never corrupt results. Null until a
	// scorekeeper pushes the first update.
	LiveTeam1 *int `json:"liveTeam1,omitempty"`
	LiveTeam2 *int `json:"liveTeam2,omitempty"`
	// Games is the per-game breakdown for a best-of-N match (omitted for legacy
	// single-game matches scored before per-game tracking).
	Games      []GameScore `json:"games,omitempty"`
	Status     string      `json:"status"`
	ResultType string      `json:"resultType,omitempty"` // normal | forfeit | retire | walkover
	// CountsForDiff = this result counts toward point differential. False for a
	// fabricated forfeit/walkover/retire-without-score; the app mirrors it so
	// per-session standings match the cumulative Leaderboard.
	CountsForDiff bool `json:"countsForDiff"`
	// Round context — populated by the event-wide pool-matches query so the
	// Game tab can group + filter every match from one stream.
	RoundID        *string `json:"roundId,omitempty"`
	RoundNumber    *int    `json:"roundNumber,omitempty"`
	RoundStatus    string  `json:"roundStatus,omitempty"`
	RoundStartedAt *string `json:"roundStartedAt,omitempty"` // when the round went active; for live "time left"
	RoundCreatedAt *string `json:"roundCreatedAt,omitempty"` // when the round was generated; groups perpetual-league games by session/date
	CompletedAt    *string `json:"completedAt,omitempty"`    // actual finish time (RFC3339 UTC); null until scored/forfeited
	LineType       string  `json:"lineType,omitempty"`       // MLP tie line: wd|md|mx1|mx2|dec
	Sides          []Side  `json:"sides"`
}

type RoundView struct {
	ID          string  `json:"id"`
	BracketID   *string `json:"bracketId,omitempty"`
	RoundNumber int     `json:"roundNumber"`
	Status      string  `json:"status"`
}

// ScheduleStatus is the organizer's "are we on time?" read-out for an in-flight
// event. Behind-ness is projected from remaining matches ÷ courts × slot length
// vs the planned finish (start + total matches ÷ courts × slot). Only meaningful
// while the event is in_progress AND has a start time; otherwise ShowFlag=false.
type ScheduleStatus struct {
	InProgress    bool   `json:"inProgress"`
	Behind        bool   `json:"behind"`
	BehindMinutes int    `json:"behindMinutes"`
	Total         int    `json:"total"`
	Completed     int    `json:"completed"`
	Remaining     int    `json:"remaining"`
	NumCourts     int    `json:"numCourts"`
	PlannedEnd    string `json:"plannedEnd,omitempty"`   // RFC3339 UTC
	ProjectedEnd  string `json:"projectedEnd,omitempty"` // RFC3339 UTC
	Affected      int    `json:"affected"`               // distinct players in unfinished matches
	AckMinutes    int    `json:"ackMinutes"`             // last acknowledged delay (0 = never)
	// ShowFlag: behind past the threshold AND grown ≥ threshold beyond the last
	// acknowledgement — the signal the banner keys off so an ack silences it
	// until the delay meaningfully worsens.
	ShowFlag bool `json:"showFlag"`
}

// MusicTrack is one entry in an event's Spotify jukebox queue. status is one of
// pending (awaiting organizer approval) | queued (ready) | playing | played |
// skipped. Tokens/URIs to control playback live server-side, never here.
type MusicTrack struct {
	ID          string `json:"id"`
	TrackURI    string `json:"trackUri"`
	TrackName   string `json:"trackName"`
	Artist      string `json:"artist,omitempty"`
	AlbumArt    string `json:"albumArt,omitempty"`
	DurationMs  int    `json:"durationMs,omitempty"`
	AddedByName string `json:"addedByName,omitempty"`
	Status      string `json:"status"`
	CreatedAt   string `json:"createdAt,omitempty"`
	// IsMine: this track was added by the calling player (lets them remove their
	// own request). Set per-request; never stored.
	IsMine bool `json:"isMine,omitempty"`
}

// AddTrackRequest is a player adding a searched Spotify track to the queue.
type AddTrackRequest struct {
	TrackURI   string `json:"trackUri"`
	TrackName  string `json:"trackName"`
	Artist     string `json:"artist"`
	AlbumArt   string `json:"albumArt"`
	DurationMs int    `json:"durationMs"`
}

type Standing struct {
	PlayerID      string `json:"playerId"`
	FullName      string `json:"fullName"`
	GamesPlayed   int    `json:"gamesPlayed"`
	Wins          int    `json:"wins"`
	Losses        int    `json:"losses"`
	PointsFor     int    `json:"pointsFor"`
	PointsAgainst int    `json:"pointsAgainst"`
	PointDiff     int    `json:"pointDiff"`
	// WinPct is wins / (wins + losses), 0..1, or 0 with no matches played.
	//
	// Ranking league play on raw WINS rewarded turning up: a 2-8 player outranked
	// a 1-1 player because losses were never compared. In a drop-in league where
	// everyone plays a different number of matches, that isn't a tiebreak detail
	// — it's the wrong primary criterion.
	WinPct float64 `json:"winPct"`
}

// SeasonSnapshot is a perpetual league's archived season: the frozen final
// standings plus the window it covered. Standings is empty in list responses.
type SeasonSnapshot struct {
	SeasonNumber int        `json:"seasonNumber"`
	StartedAt    string     `json:"startedAt,omitempty"`
	EndedAt      string     `json:"endedAt,omitempty"`
	Standings    []Standing `json:"standings,omitempty"`
}

// ---- request DTOs ----

type BracketInput struct {
	// ID is set ONLY by the edit-tournament sync flow to update an EXISTING
	// division; empty means "create a new division". Ignored on create.
	ID           string   `json:"id,omitempty"`
	Name         string   `json:"name"`
	MinRating    *float64 `json:"minRating,omitempty"`
	MaxRating    *float64 `json:"maxRating,omitempty"`
	MinAge       *int     `json:"minAge,omitempty"`
	MaxAge       *int     `json:"maxAge,omitempty"`
	DivisionType string   `json:"divisionType"` // default "open" (see Bracket)
	DuprMin      *float64 `json:"duprMin,omitempty"`
	DuprMax      *float64 `json:"duprMax,omitempty"`
	// Courts pins this division to specific court numbers (empty = any court).
	Courts []int `json:"courts,omitempty"`
	// PlayerCount: expected players, for placeholder-player schedule previews.
	PlayerCount *int `json:"playerCount,omitempty"`
	// StartMinutes: wave start as minute-of-day (480 = 8am). nil = event start.
	StartMinutes *int `json:"startMinutes,omitempty"`
	// TeamCount / PlayersPerTeam: team (MLP) division config.
	TeamCount      *int `json:"teamCount,omitempty"`
	PlayersPerTeam *int `json:"playersPerTeam,omitempty"`
}

// PlayoffSeed is one team in playoff seed order — its players (ids + names) and
// combined pool record — so the organizer can review/reorder before building.
type PlayoffSeed struct {
	PlayerIDs []string `json:"playerIds"`
	Names     []string `json:"names"`
	Wins      int      `json:"wins"`
	PointDiff int      `json:"pointDiff"`
	PointsFor int      `json:"pointsFor"`
}

// PlayoffSeedInfo is the Build-playoff dialog's payload: the seeded teams plus
// pool progress, so the dialog can gate draw size by team count and warn /
// disable Build until the pool matches are finished.
type PlayoffSeedInfo struct {
	Teams      []PlayoffSeed `json:"teams"`
	PoolsTotal int           `json:"poolsTotal"`
	PoolsOpen  int           `json:"poolsOpen"`
}

// ScheduleResult is the build-schedule response: how many matches were created,
// plus any doubles players left without a partner (an odd field leaves one out)
// so the organizer is told instead of the player being silently dropped.
type ScheduleResult struct {
	Matches     int      `json:"matches"`
	Unscheduled []string `json:"unscheduled"`
}

// DuprConnectInput is what the frontend sends after the SSO iframe posts back —
// the user's DUPR id + tokens (and any ratings carried in the SSO `stats`).
type DuprConnectInput struct {
	DuprID        string   `json:"duprId"`
	UserToken     string   `json:"userToken"`
	RefreshToken  string   `json:"refreshToken"`
	DoublesRating *float64 `json:"doublesRating"`
	SinglesRating *float64 `json:"singlesRating"`
}

// DuprConnection is the public (token-free) view of a user's DUPR link, for
// showing "DUPR connected" + the rating on their profile.
type DuprConnection struct {
	Connected     bool     `json:"connected"`
	DuprID        string   `json:"duprId,omitempty"`
	DoublesRating *float64 `json:"doublesRating,omitempty"`
	SinglesRating *float64 `json:"singlesRating,omitempty"`
	ConnectedAt   string   `json:"connectedAt,omitempty"`
}

type CreateEventRequest struct {
	Name                 string `json:"name"`
	Format               string `json:"format"`           // singles|doubles (default doubles)
	PartnerMode          string `json:"partnerMode"`      // fixed|rotating (default rotating)
	TournamentFormat     string `json:"tournamentFormat"` // default round_robin
	ScoringMode          string `json:"scoringMode"`      // default wins
	NumCourts            int    `json:"numCourts"`
	PointsToWin          int    `json:"pointsToWin"`
	WinBy                int    `json:"winBy"`
	BestOf               int    `json:"bestOf"`
	GameDurationMinutes  int    `json:"gameDurationMinutes"`
	MinPoolRounds        int    `json:"minPoolRounds"`
	MaxPoolRounds        int    `json:"maxPoolRounds"`
	RoundsPerSession     int    `json:"roundsPerSession"`
	RsvpEnabled          bool   `json:"rsvpEnabled"`
	RegistrationFeeCents int    `json:"registrationFeeCents"`
	// Currency the fees are collected in (ISO-4217, e.g. USD/CAD/PHP/GBP/AUD).
	// Empty on update = leave unchanged; empty on create defaults to USD.
	Currency string `json:"currency"`
	// Multi-division pricing (see CreateEventRequest).
	ExtraDivisionFeeMode       string `json:"extraDivisionFeeMode"`
	AdditionalDivisionFeeCents int    `json:"additionalDivisionFeeCents"`
	AddonTeeCents              int    `json:"addonTeeCents,omitempty"`
	AddonGripsCents            int    `json:"addonGripsCents,omitempty"`
	// Event-tee presale (custom merch) config.
	AddonTeeName        string   `json:"addonTeeName,omitempty"`
	AddonTeeFrontURL    string   `json:"addonTeeFrontUrl,omitempty"`
	AddonTeeBackURL     string   `json:"addonTeeBackUrl,omitempty"`
	AddonTeeSizes       []string `json:"addonTeeSizes,omitempty"`
	ZelleHandle         string   `json:"zelleHandle,omitempty"`
	VenmoHandle         string   `json:"venmoHandle,omitempty"`
	GcashHandle         string   `json:"gcashHandle,omitempty"`
	ClubID              string   `json:"clubId,omitempty"`
	Location            string   `json:"location"`
	ContactPhone        string   `json:"contactPhone"`
	VenueNotes          string   `json:"venueNotes"`
	WaiverURL           string   `json:"waiverUrl"`
	ConfirmEmailSubject string   `json:"confirmEmailSubject"`
	ConfirmEmailMessage string   `json:"confirmEmailMessage"`
	// Organizer email branding (Premium) — applied to every outgoing email for
	// this event when the owner is Premium. EmailBrandColor is a #RRGGBB accent;
	// EmailSignature is a plain-text sign-off. Pointers so the write path can tell
	// "field omitted → leave the stored value alone" (nil) from "field sent empty
	// → clear back to the default" (""). Never NULL a value the client didn't send.
	EmailBrandLogoURL *string `json:"emailBrandLogoUrl,omitempty"`
	EmailBrandColor   *string `json:"emailBrandColor,omitempty"`
	EmailSignature    *string `json:"emailSignature,omitempty"`
	// RatingEnforcement: "off" | "warn" | "block" (anti-sandbagging DUPR gating).
	RatingEnforcement string   `json:"ratingEnforcement,omitempty"`
	VenueName         string   `json:"venueName"`
	VenueAddress      string   `json:"venueAddress"`
	VenuePhone        string   `json:"venuePhone"`
	VenueWebsite      string   `json:"venueWebsite"`
	VenueLat          *float64 `json:"venueLat"`
	VenueLng          *float64 `json:"venueLng"`
	DuprSanctioned    bool     `json:"duprSanctioned"`
	// DuprMinEntitlement: "" | "DUPR_PLUS" — gates self-register on a DUPR+
	// membership (Premium + Verified). Non-empty implies DuprSanctioned.
	DuprMinEntitlement  string         `json:"duprMinEntitlement"`
	CashPrize           bool           `json:"cashPrize"`
	CashPrizeAmount     *float64       `json:"cashPrizeAmount,omitempty"`
	Consolation         bool           `json:"consolation"` // single_elim back-draw
	AutoAdjust          bool           `json:"autoAdjust"`
	AutoStartNext       bool           `json:"autoStartNext"` // auto-start next game on a freed court
	CourtCalls          bool           `json:"courtCalls"`    // TV scoreboard defaults voice court-calls on
	TeamSize            int            `json:"teamSize"`      // >0 = MLP team event (4 = 2M/2W)
	StartsAt            string         `json:"startsAt"`      // RFC3339 UTC, "" = none
	EndsAt              string         `json:"endsAt"`        // RFC3339 UTC, "" = none
	Description         string         `json:"description"`
	AdminPasscode       string         `json:"adminPasscode"`
	Brackets            []BracketInput `json:"brackets"`
	Listed              bool           `json:"listed"`
	PlayerScoring       bool           `json:"playerScoring"`
	ScoreConfirmMinutes int            `json:"scoreConfirmMinutes"`
	SmsNotifications    bool           `json:"smsNotifications"`
	OnDeckSms           bool           `json:"onDeckSms"`
	MaxPlayers          *int           `json:"maxPlayers,omitempty"`
	RegistrationCloseAt *string        `json:"registrationCloseAt,omitempty"`
	PosterURL           string         `json:"posterUrl"`
	// RequireApproval gates self-registrations behind organizer approval.
	RequireApproval bool `json:"requireApproval"`
	// CourtScorePasscode toggles requiring the scorekeeper passcode on court-QR
	// scoring. Pointer so an omitting client keeps the DB default (on) on create
	// and leaves it unchanged on update.
	CourtScorePasscode *bool `json:"courtScorePasscode"`
	// RegistrationCode is the invite code self-registrants must supply. Pointer so
	// edit can distinguish the three cases: nil (omitted) = keep the current code,
	// "" = clear it (open registration), any value = set it.
	RegistrationCode *string `json:"registrationCode,omitempty"`
	// Recurring "socials": RecurIntervalDays>0 makes this event the head of a
	// series that auto-spawns the next occurrence every N days (7=weekly,
	// 14=biweekly, custom otherwise). RecurUntil (RFC3339, "" = open-ended) caps
	// how far out the series generates.
	RecurIntervalDays int    `json:"recurIntervalDays,omitempty"`
	RecurUntil        string `json:"recurUntil,omitempty"`
}

type RegisterRequest struct {
	FullName        string   `json:"fullName"`
	Phone           string   `json:"phone"`
	Email           string   `json:"email"`
	SkillLevel      *float64 `json:"skillLevel,omitempty"`
	DuprID          string   `json:"duprId"`
	DuprRating      *float64 `json:"duprRating,omitempty"`
	DuprReliability *float64 `json:"duprReliability,omitempty"`
	PartnerID       string   `json:"partnerId"`
	// PartnerName / PartnerPhone let a registrant sign up WITH a partner in one
	// step. When PartnerPhone matches an existing registrant, they're paired (no
	// duplicate); otherwise the partner is added to the roster and cross-linked.
	PartnerName  string `json:"partnerName"`
	PartnerPhone string `json:"partnerPhone"`
	BracketID    string `json:"bracketId"`
	// SmsConsent is the player's explicit opt-in to automated texts (court calls,
	// schedule alerts, score confirmations). Distinct from Phone: the phone is
	// stored regardless (organizers need it) but is only texted when this is true.
	SmsConsent bool `json:"smsConsent"`
	// Self is true only when a LOGGED-IN user is registering THEMSELVES (the
	// self-registration flow). It links the player to their account
	// (players.user_id); an organizer adding other players leaves it false.
	Self bool `json:"self"`
	// CaptchaToken is a Cloudflare Turnstile token sent only by the PUBLIC
	// self-registration form (anonymous). The handler verifies it server-side;
	// the service ignores it.
	CaptchaToken string `json:"captchaToken,omitempty"`
	// RegistrationCode is the invite code a self-registrant must supply when the
	// event has one set (events.registration_code). Ignored for owner-adds.
	RegistrationCode string `json:"registrationCode,omitempty"`
	// TrustedAdd is a SERVER-ONLY flag (json:"-" so a client can never inject it):
	// the register handler sets it true only when an AUTHENTICATED event OWNER is
	// adding a player. It gates the contact→account matcher (accountForContact) so
	// an anonymous/self-service registrant can't force-link to a stranger's
	// account. `Self` (client-controlled) must NOT be used for this.
	TrustedAdd bool `json:"-"`
	// AllowNoContact (server-only) waives the no-phone/no-email cap for this
	// registration. Set for ONE-NIGHT substitutes: a walk-in standing in for
	// tonight doesn't need to be reachable between sessions the way a season
	// player does, and typing just their name is the whole point.
	AllowNoContact bool `json:"-"`
	// SkipCoachEnroll (server-only) suppresses the coach-led auto-enroll for this
	// registration — set for temporary substitutes, who shouldn't become permanent
	// coaching students.
	SkipCoachEnroll bool `json:"-"`
}

// VideoAnalysis is one paid Match Video Analysis (PB Vision). Status flows
// pending_payment -> processing -> ready | failed. ReportURL is the hosted PB
// Vision report, filled in by the completion webhook.
type VideoAnalysis struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Name        string `json:"name,omitempty"`
	Court       string `json:"court,omitempty"`
	ReportURL   string `json:"reportUrl,omitempty"`
	Error       string `json:"error,omitempty"`
	AmountCents int    `json:"amountCents"`
	Currency    string `json:"currency,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	// Insights/Stats are PB Vision's full analysis JSON — only populated by
	// GetAnalysis (the detail fetch), so the list stays light. The client renders
	// a summary from these; the hosted report (ReportURL) is the full display.
	Insights any `json:"insights,omitempty"`
	Stats    any `json:"stats,omitempty"`
}

// AnalysisCheckoutRequest starts a paid analysis: the uploaded video's public URL
// plus optional labels + player emails (for PB Vision to attach the report to).
type AnalysisCheckoutRequest struct {
	VideoURL      string   `json:"videoUrl"`
	Name          string   `json:"name"`
	Court         string   `json:"court"`
	PartnerEmails []string `json:"partnerEmails"`
	SuccessURL    string   `json:"successUrl"`
	CancelURL     string   `json:"cancelUrl"`
}

// --- Instructor Mode / Coaching (Phase 1) ---

// CoachStudent is one coach↔student relationship (the roster row). Its ID doubles
// as the thread id that videos + feedback hang off of. StudentID is the resolved
// account id (may be empty until the student logs in). VideoCount is a convenience
// count returned on list views.
type CoachStudent struct {
	ID        string `json:"id"`
	CoachID   string `json:"coachId,omitempty"`
	CoachName string `json:"coachName,omitempty"`
	// CoachPhotoURL is the coach's avatar (coach profile photo, else account
	// avatar) — populated only on the student's "My coaching" list for a richer
	// card. LastActivity is the newest thread activity as an ISO timestamp
	// (the frontend renders it as "3 days ago"); mirrors the internal
	// LastActivityAt but is safe to serialize.
	CoachPhotoURL  string `json:"coachPhotoUrl,omitempty"`
	LastActivity   string `json:"lastActivity,omitempty"`
	StudentEmail   string `json:"studentEmail,omitempty"`
	StudentPhone   string `json:"studentPhone,omitempty"`
	StudentName    string `json:"studentName,omitempty"`
	StudentID      string `json:"studentId,omitempty"`
	SkillLevel     string `json:"skillLevel,omitempty"`
	VideoCount     int    `json:"videoCount"`
	HasUnread      bool   `json:"hasUnread"`
	CreatedAt      string `json:"createdAt,omitempty"`
	LastActivityAt string `json:"-"` // internal: drives HasUnread, not sent raw
	// RubricAvg is the student's average skill-ratings score (nil when unrated);
	// OpenGoals is how many assigned drills aren't done yet; AwaitingFeedback is
	// how many of the student's own clips have no coach comment yet. All three are
	// coach-facing roster aggregates so a coach can scan who needs attention.
	RubricAvg        *float64 `json:"rubricAvg,omitempty"`
	OpenGoals        int      `json:"openGoals"`
	AwaitingFeedback int      `json:"awaitingFeedback"`
	// CoachNote is the coach's private running note about the student — only
	// populated for the coach's own views, redacted from the student's view.
	CoachNote string `json:"coachNote,omitempty"`
	// SharedNote is a note the coach writes FOR the student to see (not redacted).
	// Deprecated by CoachingSharedNote (the titled/dated list); kept for back-compat.
	SharedNote string `json:"sharedNote,omitempty"`
}

// CoachingSharedNote is one titled, dated note the coach posts for a student to
// see. Editable is true only within 24h of posting (server-authoritative).
type CoachingSharedNote struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Editable  bool   `json:"editable"`
}

// SharedNoteRequest creates/edits a shared note.
type SharedNoteRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// CoachingVideo is a clip in a thread, with its feedback nested for the detail view.
type CoachingVideo struct {
	ID             string             `json:"id"`
	CoachStudentID string             `json:"coachStudentId,omitempty"`
	UploadedBy     string             `json:"uploadedBy,omitempty"`
	UploaderRole   string             `json:"uploaderRole,omitempty"`
	UploaderName   string             `json:"uploaderName,omitempty"`
	VideoURL       string             `json:"videoUrl"`
	Title          string             `json:"title,omitempty"`
	CreatedAt      string             `json:"createdAt,omitempty"`
	Feedback       []CoachingFeedback `json:"feedback,omitempty"`
	// Source is "upload" (coach/student) or "pbvision" (auto-imported highlight).
	Source string `json:"source,omitempty"`
	// PBVisionStatus is the state of a PB Vision analysis launched from this clip:
	// "" (none), "processing", "ready", or "failed".
	PBVisionStatus string `json:"pbVisionStatus,omitempty"`
	// CoachNote is the coach's PRIVATE note on this clip — only populated when the
	// viewer is the coach; never sent to the student.
	CoachNote string `json:"coachNote,omitempty"`
}

// CoachingFeedback is one text comment, authored by the coach or the student.
type CoachingFeedback struct {
	ID             string `json:"id"`
	CoachStudentID string `json:"coachStudentId,omitempty"`
	VideoID        string `json:"videoId,omitempty"`
	AuthorID       string `json:"authorId,omitempty"`
	AuthorRole     string `json:"authorRole,omitempty"`
	AuthorName     string `json:"authorName,omitempty"`
	Body           string `json:"body"`
	CreatedAt      string `json:"createdAt,omitempty"`
	// TimestampSeconds pins the comment to a moment in the clip (nullable).
	TimestampSeconds *float64 `json:"timestampSeconds,omitempty"`
	// Annotation is an optional telestration overlay for the pinned moment: a
	// {strokes:[{tool,color,points:[[x,y]...]}]} blob with NORMALIZED (0..1)
	// coordinates, re-drawn over the clip when the viewer seeks to the moment.
	Annotation any `json:"annotation,omitempty"`
}

// CoachingThread is a roster row plus its clips (each with nested feedback).
type CoachingThread struct {
	Student CoachStudent    `json:"student"`
	Videos  []CoachingVideo `json:"videos"`
	// Per-coach section visibility (from coach_settings). Both the coach's and
	// the student's Goals tab honor these, so a coach who doesn't use skill
	// ratings / progress / achievements can hide those cards. Default true.
	ShowProgress     bool `json:"showProgress"`
	ShowAchievements bool `json:"showAchievements"`
	ShowSkillRatings bool `json:"showSkillRatings"`
}

// CoachSettings is a coach's per-account preferences (currently which Goals-tab
// sections they use). Missing row / pre-migration DB ⇒ all true.
type CoachSettings struct {
	ShowProgress     bool `json:"showProgress"`
	ShowAchievements bool `json:"showAchievements"`
	ShowSkillRatings bool `json:"showSkillRatings"`
}

// CoachingScheduleItem is one entry on the coach's schedule: a booked lesson
// ("session"), offered availability ("open"), or unavailable time ("blocked").
type CoachingScheduleItem struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"` // session | open | blocked
	CoachStudentID string `json:"coachStudentId,omitempty"`
	StudentLabel   string `json:"studentLabel,omitempty"`
	StartsAt       string `json:"startsAt"`
	EndsAt         string `json:"endsAt,omitempty"`
	AllDay         bool   `json:"allDay"`
	Location       string `json:"location,omitempty"`
	Notes          string `json:"notes,omitempty"`
	// CoachName is populated only on a player's own booked-sessions list.
	CoachName string `json:"coachName,omitempty"`
	// CoachID is the coach's user id (so a player can fetch that coach's open
	// availability to reschedule into).
	CoachID string `json:"coachId,omitempty"`
	// Status is a session's attendance: "" (unmarked), "attended", "no_show".
	Status string `json:"status,omitempty"`
}

// CoachingMessage is one free-form chat message on a coaching thread.
type CoachingMessage struct {
	ID         string `json:"id"`
	SenderID   string `json:"senderId"`
	SenderRole string `json:"senderRole"` // coach | student
	Body       string `json:"body"`
	CreatedAt  string `json:"createdAt"`
}

// CoachingMessageRequest sends a chat message on a thread.
type CoachingMessageRequest struct {
	Body string `json:"body"`
}

// CoachBookingRequest is a player booking a 1:1 session inside a coach's open
// availability window.
type CoachBookingRequest struct {
	StartsAt     string `json:"startsAt"`
	DurationMins int    `json:"durationMins"`
	Location     string `json:"location"`
	// WhatToWorkOn is the player's optional agenda for the session; stored as the
	// session notes so the coach sees the goal before they arrive.
	WhatToWorkOn string `json:"whatToWorkOn"`
}

// CoachingScheduleRequest books/opens/blocks a schedule entry.
type CoachingScheduleRequest struct {
	Kind           string `json:"kind"`
	CoachStudentID string `json:"coachStudentId"`
	StudentLabel   string `json:"studentLabel"`
	StartsAt       string `json:"startsAt"`
	EndsAt         string `json:"endsAt"`
	AllDay         bool   `json:"allDay"`
	Location       string `json:"location"`
	Notes          string `json:"notes"`
	// RepeatWeeks, when > 1 on an open/blocked window, also creates that many
	// weekly copies (this week + the next RepeatWeeks-1). Ignored for sessions.
	RepeatWeeks int `json:"repeatWeeks"`
}

// CoachingDrill is one drill in a coach's library — either a shared starter drill
// (IsStarter, no CoachID) or the coach's own custom drill.
type CoachingDrill struct {
	ID            string `json:"id"`
	CoachID       string `json:"coachId,omitempty"`
	Title         string `json:"title"`
	SkillCategory string `json:"skillCategory,omitempty"`
	LevelBand     string `json:"levelBand,omitempty"`
	Format        string `json:"format,omitempty"`
	Goal          string `json:"goal,omitempty"`
	Description   string `json:"description,omitempty"`
	VideoURL      string `json:"videoUrl,omitempty"`
	IsStarter     bool   `json:"isStarter"`
	CreatedAt     string `json:"createdAt,omitempty"`
}

// CoachingDrillRequest creates a coach's custom drill.
type CoachingDrillRequest struct {
	Title         string `json:"title"`
	SkillCategory string `json:"skillCategory"`
	LevelBand     string `json:"levelBand"`
	Format        string `json:"format"`
	Goal          string `json:"goal"`
	Description   string `json:"description"`
	VideoURL      string `json:"videoUrl"`
}

// CoachingAssignment is a drill assigned to a roster student — a goal on their
// game plan. Drill fields are snapshotted so the source drill can change freely.
type CoachingAssignment struct {
	ID             string `json:"id"`
	CoachStudentID string `json:"coachStudentId,omitempty"`
	DrillID        string `json:"drillId,omitempty"`
	Title          string `json:"title"`
	SkillCategory  string `json:"skillCategory,omitempty"`
	Goal           string `json:"goal,omitempty"`
	Status         string `json:"status"` // assigned | in_progress | done
	DueAt          string `json:"dueAt,omitempty"`
	CompletedAt    string `json:"completedAt,omitempty"`
	CompletedBy    string `json:"completedBy,omitempty"`
	CreatedAt      string `json:"createdAt,omitempty"`
}

// AssignDrillRequest assigns a drill (by id) or an ad-hoc goal (title/goal) to a
// roster student.
type AssignDrillRequest struct {
	DrillID       string `json:"drillId"`
	Title         string `json:"title"`
	SkillCategory string `json:"skillCategory"`
	Goal          string `json:"goal"`
	DueAt         string `json:"dueAt"`
}

// CompleteAssignmentRequest marks an assignment done (or reopens it).
type CompleteAssignmentRequest struct {
	Done bool `json:"done"`
}

// CoachProfile is a coach's public discovery profile (marketplace). DistanceKm
// is only populated on nearby-search results.
type CoachProfile struct {
	UserID          string   `json:"userId,omitempty"`
	Name            string   `json:"name,omitempty"`
	Listed          bool     `json:"listed"`
	Bio             string   `json:"bio,omitempty"`
	YearsExperience *int     `json:"yearsExperience,omitempty"`
	BusinessName    string   `json:"businessName,omitempty"`
	Address         string   `json:"address,omitempty"`
	City            string   `json:"city,omitempty"`
	Lat             *float64 `json:"lat,omitempty"`
	Lng             *float64 `json:"lng,omitempty"`
	HourlyRateCents *int     `json:"hourlyRateCents,omitempty"`
	Skills          string   `json:"skills,omitempty"`
	PhotoURL        string   `json:"photoUrl,omitempty"`
	DistanceKm      *float64 `json:"distanceKm,omitempty"`
	RatingAvg       *float64 `json:"ratingAvg,omitempty"`
	RatingCount     int      `json:"ratingCount"`
	// HasIntroVideo is true when the coach has recorded an intro clip (the raw
	// bucket path is never exposed — playback is via a signed-URL endpoint).
	HasIntroVideo bool `json:"hasIntroVideo"`
	// CancelPolicy: flexible | moderate | strict.
	CancelPolicy string `json:"cancelPolicy,omitempty"`
	// Favorited is true when the requesting viewer has saved this coach.
	Favorited bool `json:"favorited"`
	// Verified is granted after vetting; Certifications is coach-entered.
	Verified       bool   `json:"verified"`
	Certifications string `json:"certifications,omitempty"`
}

// CoachReview is a player's star rating + comment for a coach. One per
// (coach, author); eligibility (trained with the coach) is enforced server-side.
type CoachReview struct {
	ID          string `json:"id"`
	CoachUserID string `json:"coachUserId"`
	AuthorID    string `json:"authorId"`
	AuthorName  string `json:"authorName"`
	Rating      int    `json:"rating"`
	Body        string `json:"body,omitempty"`
	// CoachResponse is the reviewed coach's public reply, if any.
	CoachResponse string `json:"coachResponse,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

// CoachReviewRequest submits/updates the caller's review of a coach.
type CoachReviewRequest struct {
	Rating int    `json:"rating"`
	Body   string `json:"body"`
}

// CoachReviewsResponse is the public reviews payload for a coach: the list, the
// aggregate, and whether the caller may write one (+ their existing review).
type CoachReviewsResponse struct {
	Reviews     []CoachReview `json:"reviews"`
	RatingAvg   *float64      `json:"ratingAvg,omitempty"`
	RatingCount int           `json:"ratingCount"`
	CanReview   bool          `json:"canReview"`
	MyReview    *CoachReview  `json:"myReview,omitempty"`
}

// PBVisionStats is a student's PB Vision AI game-report analytics for a thread.
// Stats is the flexible metric blob (shot quality, speeds, kitchen arrival, shot
// mix, strengths/improve, etc.); Connected is false when no report is synced yet.
type PBVisionStats struct {
	Connected    bool           `json:"connected"`
	Rating       *float64       `json:"rating,omitempty"`
	LastSyncedAt string         `json:"lastSyncedAt,omitempty"`
	Stats        map[string]any `json:"stats,omitempty"`
}

// PBVisionAnalysis is a completed PB Vision analysis for a thread's latest
// analyzed clip: the detected players (for the "which player are you?" picker)
// plus which one the student tagged. Empty (Ready=false) when nothing's ready.
type PBVisionAnalysis struct {
	Ready     bool             `json:"ready"`
	JobID     string           `json:"jobId,omitempty"`
	ReportURL string           `json:"reportUrl,omitempty"`
	TaggedID  *int             `json:"taggedAvatarId,omitempty"`
	Players   []PBVisionPlayer `json:"players,omitempty"`
	// SourceVideoURL is a signed playback URL for the analyzed clip (so the
	// highlights, which are time-ranges into it, can be played inline).
	SourceVideoURL string `json:"sourceVideoUrl,omitempty"`
	// SourceVideoID is the coaching_videos id of the analyzed clip, so a coach
	// can pin feedback onto a highlight (feedback attaches to that clip).
	SourceVideoID string `json:"sourceVideoId,omitempty"`
	// ThreadID is the coach_students thread this analysis lives in (the buyer's
	// thread), and BuyerName is that student — so a coach-level list can label
	// each analysis "Paid by <buyer>" and route assignments to the right thread.
	ThreadID  string `json:"threadId,omitempty"`
	BuyerName string `json:"buyerName,omitempty"`
	// CreatedAt is when the analysis became ready (for a date label).
	CreatedAt  string              `json:"createdAt,omitempty"`
	Highlights []PBVisionHighlight `json:"highlights,omitempty"`
	// ViewerRole is "coach" or "student". A coach sees Roster + Assignments to
	// distribute the 4 detected players across their students; a student only
	// sees their own tagged player (TaggedID).
	ViewerRole  string               `json:"viewerRole,omitempty"`
	Roster      []CoachStudentBrief  `json:"roster,omitempty"`
	Assignments []PBVisionAssignment `json:"assignments,omitempty"`
	// MatchStats is the advanced team/player breakdown parsed from PB Vision's
	// stats.json (kitchen arrival, shot distribution, rallies won, …).
	MatchStats *PBVisionMatchStats `json:"matchStats,omitempty"`
}

// PBVisionMatchStats mirrors PB Vision's Team-Stats page from the stats.json
// payload. Percentages are 0–100. Players are index 0–3 = avatar 0–3 (P1–P4).
type PBVisionMatchStats struct {
	Score            []int                `json:"score,omitempty"`            // [A,B]
	TeamPctToKitchen []float64            `json:"teamPctToKitchen,omitempty"` // [A,B]
	AvgShots         float64              `json:"avgShots,omitempty"`
	LongestRally     int                  `json:"longestRally,omitempty"`
	KitchenRallies   int                  `json:"kitchenRallies,omitempty"`
	RalliesWon       []PBVisionRallyBand  `json:"ralliesWon,omitempty"`
	Players          []PBVisionPlayerStat `json:"players,omitempty"`
}

// PBVisionRallyBand = who won what share of rallies of a given length.
type PBVisionRallyBand struct {
	Label string  `json:"label"`
	TeamA float64 `json:"teamA"`
	TeamB float64 `json:"teamB"`
}

// PBVisionPlayerStat = one detected player's team-stats row.
type PBVisionPlayerStat struct {
	AvatarID         int                `json:"avatarId"`
	Team             int                `json:"team"`
	ServeKitchenPct  float64            `json:"serveKitchenPct"`
	ReturnKitchenPct float64            `json:"returnKitchenPct"`
	ShotSharePct     float64            `json:"shotSharePct"`
	LeftSidePct      float64            `json:"leftSidePct"`
	Speedups         int                `json:"speedups"`
	ShotCount        int                `json:"shotCount"`
	ShotQuality      float64            `json:"shotQuality"`
	Shots            []PBVisionShotStat `json:"shots,omitempty"`
}

// PBVisionShotStat = one shot type's per-player breakdown (the "data nerds" view).
type PBVisionShotStat struct {
	Type       string  `json:"type"`
	Count      int     `json:"count"`
	SuccessPct float64 `json:"successPct"`
	WonPct     float64 `json:"wonPct"`  // rally_won_percentage
	Quality    float64 `json:"quality"` // 0–1
	AvgSpeed   float64 `json:"avgSpeed"`
}

// CoachStudentBrief is a coach's student, for the assign dropdown.
type CoachStudentBrief struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

// PBVisionAssignment maps a detected player to a student for one analysis.
type PBVisionAssignment struct {
	AvatarID        int    `json:"avatarId"`
	StudentThreadID string `json:"studentThreadId"`
	StudentName     string `json:"studentName"`
}

// PBVisionHighlight is one notable moment PB Vision flagged: a time-range
// (StartSeconds..EndSeconds) into the source clip, with a label.
type PBVisionHighlight struct {
	StartSeconds float64 `json:"startSeconds"`
	EndSeconds   float64 `json:"endSeconds"`
	Kind         string  `json:"kind,omitempty"`
	Title        string  `json:"title,omitempty"`
}

// PBVisionPlayer is one detected on-court player. AvatarID is PB Vision's
// per-video 0..3 index; Label is a human hint ("Team A · Player 1", matching the
// badge + PB Vision's report) since the payload carries no name or photo. Stats
// holds that player's metrics.
type PBVisionPlayer struct {
	AvatarID int            `json:"avatarId"`
	Team     int            `json:"team"`
	Label    string         `json:"label"`
	Stats    map[string]any `json:"stats,omitempty"`
}

// CoachProgramTemplate is a reusable multi-week plan a coach saves once and
// applies to many students (weeks carry focus + drills, no per-student dates).
type CoachProgramTemplate struct {
	ID        string                `json:"id"`
	Title     string                `json:"title"`
	Weeks     []CoachingProgramWeek `json:"weeks"`
	CreatedAt string                `json:"createdAt,omitempty"`
}

// CoachingPracticeLog is one "I practiced" entry a student self-logs between
// sessions — the accountability/return hook.
type CoachingPracticeLog struct {
	ID       string `json:"id"`
	Note     string `json:"note,omitempty"`
	LoggedAt string `json:"loggedAt"`
	ByName   string `json:"byName,omitempty"`
}

// CoachingPracticeSummary is a thread's practice history + a consecutive-day
// streak, for the student's return hook and the coach's accountability view.
type CoachingPracticeSummary struct {
	Logs          []CoachingPracticeLog `json:"logs"`
	CurrentStreak int                   `json:"currentStreak"`
	TotalLogs     int                   `json:"totalLogs"`
	LoggedToday   bool                  `json:"loggedToday"`
}

// CoachingProgram is a multi-week training plan assigned to a student.
type CoachingProgram struct {
	ID        string                `json:"id"`
	Title     string                `json:"title"`
	Weeks     []CoachingProgramWeek `json:"weeks"`
	CreatedAt string                `json:"createdAt,omitempty"`
}

// CoachingProgramWeek is one week's focus + completion, plus an optional due
// date and any drills the coach attached to that week.
type CoachingProgramWeek struct {
	Focus  string                 `json:"focus"`
	Done   bool                   `json:"done"`
	Due    string                 `json:"due,omitempty"`
	Drills []CoachingProgramDrill `json:"drills,omitempty"`
}

// CoachingProgramDrill is a drill reference attached to a program week (a copy
// of the drill's id + title so the plan renders without a join).
type CoachingProgramDrill struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// CoachingProgramRequest creates a program (coach): title + per-week entries.
type CoachingProgramRequest struct {
	Title string                     `json:"title"`
	Weeks []CoachingProgramWeekInput `json:"weeks"`
}

// CoachingProgramWeekInput is one week as submitted by the coach: a focus line,
// an optional due date (YYYY-MM-DD or RFC3339), and optional attached drills.
type CoachingProgramWeekInput struct {
	Focus  string                 `json:"focus"`
	Due    string                 `json:"due,omitempty"`
	Drills []CoachingProgramDrill `json:"drills,omitempty"`
}

// PBVisionReport is one historical PB Vision snapshot for a thread.
type PBVisionReport struct {
	ID       string         `json:"id"`
	Rating   *float64       `json:"rating,omitempty"`
	SyncedAt string         `json:"syncedAt"`
	Stats    map[string]any `json:"stats,omitempty"`
}

// CoachProfileRequest upserts the signed-in coach's discovery profile. Location
// is geocoded from City server-side. Listing requires a bio + years of
// experience (enforced in UpsertCoachProfile).
type CoachProfileRequest struct {
	Listed          bool   `json:"listed"`
	Bio             string `json:"bio"`
	YearsExperience *int   `json:"yearsExperience"`
	BusinessName    string `json:"businessName"`
	Address         string `json:"address"`
	City            string `json:"city"`
	HourlyRateCents *int   `json:"hourlyRateCents"`
	Skills          string `json:"skills"`
	CancelPolicy    string `json:"cancelPolicy"`
	Certifications  string `json:"certifications"`
}

// CoachingClass is a group class a coach offers that players can enroll in.
type CoachingClass struct {
	ID          string `json:"id"`
	CoachID     string `json:"coachId,omitempty"`
	CoachName   string `json:"coachName,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	StartsAt    string `json:"startsAt"`
	EndsAt      string `json:"endsAt,omitempty"`
	Location    string `json:"location,omitempty"`
	// Lat/Lng pin the class on the player-facing map; DistanceKm is filled on
	// "classes near me" results (km from the viewer).
	Lat        *float64 `json:"lat,omitempty"`
	Lng        *float64 `json:"lng,omitempty"`
	DistanceKm *float64 `json:"distanceKm,omitempty"`
	Capacity   int      `json:"capacity"`
	PriceCents int      `json:"priceCents"`
	// Level: '' | beginner | intermediate | advanced.
	Level         string `json:"level,omitempty"`
	EnrolledCount int    `json:"enrolledCount"`
	WaitlistCount int    `json:"waitlistCount"`
	// Enrolled / Waitlisted / Offered reflect the caller's own status. Offered =
	// a freed paid seat awaiting claim; OfferExpiresAt is the claim deadline.
	Enrolled       bool   `json:"enrolled"`
	Waitlisted     bool   `json:"waitlisted"`
	Offered        bool   `json:"offered,omitempty"`
	OfferExpiresAt string `json:"offerExpiresAt,omitempty"`
	// CancelPolicy (flexible|moderate|strict) drives the refund/cutoff line shown
	// to players at enroll/cancel time. Empty = flexible (cancel anytime).
	CancelPolicy string `json:"cancelPolicy,omitempty"`
	IsIntro      bool   `json:"isIntro"`
	CreatedAt    string `json:"createdAt,omitempty"`
}

// CoachingEnrollment is a player's seat in a class.
type CoachingEnrollment struct {
	ID          string `json:"id"`
	ClassID     string `json:"classId,omitempty"`
	UserID      string `json:"userId,omitempty"`
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	Status      string `json:"status"`
	AmountCents int    `json:"amountCents"`
	Paid        bool   `json:"paid"`
	ChargeAt    string `json:"chargeAt,omitempty"`
	// OfferExpiresAt is set when status is 'offered' (a freed paid seat the
	// player must claim & pay before this time, or it rolls to the next person).
	OfferExpiresAt string `json:"offerExpiresAt,omitempty"`
	CreatedAt      string `json:"createdAt,omitempty"`
	// ClassTitle/StartsAt populated on a player's "my classes" list.
	ClassTitle string `json:"classTitle,omitempty"`
	StartsAt   string `json:"startsAt,omitempty"`
	CoachName  string `json:"coachName,omitempty"`
	Location   string `json:"location,omitempty"`
	// WaitlistPosition is the player's 1-based spot in the waitlist (when waitlisted).
	WaitlistPosition int `json:"waitlistPosition,omitempty"`
}

// EnrollPayRequest carries the return URLs for a class-enrollment checkout.
type EnrollPayRequest struct {
	SuccessURL string `json:"successUrl"`
	CancelURL  string `json:"cancelUrl"`
}

// CoachingClassRequest creates a class (coach only).
type CoachingClassRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	StartsAt    string `json:"startsAt"`
	EndsAt      string `json:"endsAt"`
	Location    string `json:"location"`
	// Lat/Lng are an explicit map-picker pin; when nil the service best-effort
	// geocodes Location so the class still lands on the map.
	Lat        *float64 `json:"lat"`
	Lng        *float64 `json:"lng"`
	Level      string   `json:"level"`
	Capacity   int      `json:"capacity"`
	PriceCents int      `json:"priceCents"`
	IsIntro    bool     `json:"isIntro"`
	// RepeatWeeks > 1 clones this class weekly for that many total weeks (each an
	// independent class/enrollment). 0 or 1 = a one-off class.
	RepeatWeeks int `json:"repeatWeeks"`
}

// CoachCreditOwed is one student's outstanding prepaid-class balance with a
// coach — the coach-facing view of pack liability (sessions they owe).
type CoachCreditOwed struct {
	StudentID        string `json:"studentId"`
	StudentName      string `json:"studentName"`
	CreditsRemaining int    `json:"creditsRemaining"`
}

// CoachPack is a discounted bundle of class credits a coach sells.
type CoachPack struct {
	ID         string `json:"id"`
	CoachID    string `json:"coachId,omitempty"`
	Title      string `json:"title"`
	Credits    int    `json:"credits"`
	PriceCents int    `json:"priceCents"`
	Active     bool   `json:"active"`
	CreatedAt  string `json:"createdAt,omitempty"`
}

// CoachPackRequest creates a pack (coach only).
type CoachPackRequest struct {
	Title      string `json:"title"`
	Credits    int    `json:"credits"`
	PriceCents int    `json:"priceCents"`
}

// CoachCredits is a player's remaining class-credit balance with a coach.
type CoachCredits struct {
	CoachID          string `json:"coachId"`
	CreditsRemaining int    `json:"creditsRemaining"`
}

// CoachingSkillRating is the coach's 1-5 assessment of one skill for a student.
type CoachingSkillRating struct {
	Skill       string  `json:"skill"` // serve|return|dinks|drops|volleys|strategy
	Rating      float64 `json:"rating"`
	FirstRating float64 `json:"firstRating,omitempty"`
	UpdatedAt   string  `json:"updatedAt,omitempty"`
}

// SetSkillRatingRequest sets one skill's rating (0 clears/leaves it unset).
type SetSkillRatingRequest struct {
	Skill  string  `json:"skill"`
	Rating float64 `json:"rating"`
}

// AddCoachStudentRequest adds someone to a coach's roster by email and/or phone
// (at least one). The coach can start immediately; the student links when they
// sign up with that email or phone.
type AddCoachStudentRequest struct {
	Email string `json:"email"`
	Phone string `json:"phone"`
	Name  string `json:"name"`
	Level string `json:"level"`
}

// Instructor is one coach on the allowlist (owner-managed).
type Instructor struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// AddInstructorRequest grants coach access to an email.
type AddInstructorRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// CoachingVideoRequest records an already-uploaded clip (client uploads the file
// directly to the coaching-videos bucket, then posts the resulting public URL).
type CoachingVideoRequest struct {
	VideoURL string `json:"videoUrl"`
	Title    string `json:"title"`
}

// CoachingFeedbackRequest is a text comment on a clip, optionally pinned to a
// moment in the video (TimestampSeconds).
type CoachingFeedbackRequest struct {
	Body             string   `json:"body"`
	TimestampSeconds *float64 `json:"timestampSeconds"`
	Annotation       any      `json:"annotation"`
}

// WaitlistEntry is one person on an event's waitlist (used when the event is at
// its MaxPlayers cap). Kept separate from registrations so it never touches
// standings/scheduling/counts until an organizer promotes it.
type WaitlistEntry struct {
	ID        string `json:"id"`
	EventID   string `json:"eventId"`
	BracketID string `json:"bracketId,omitempty"`
	Division  string `json:"division,omitempty"`
	FullName  string `json:"fullName"`
	Phone     string `json:"phone,omitempty"`
	Email     string `json:"email,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// WaitlistJoinRequest is the public "join the waitlist" payload.
type WaitlistJoinRequest struct {
	FullName     string   `json:"fullName"`
	Phone        string   `json:"phone"`
	Email        string   `json:"email"`
	BracketID    string   `json:"bracketId"`
	SkillLevel   *float64 `json:"skillLevel,omitempty"`
	SmsConsent   bool     `json:"smsConsent"`
	CaptchaToken string   `json:"captchaToken,omitempty"`
}

// ImportRosterRequest bulk-registers many players into an event from a roster
// import (owner-only). All players go into the chosen BracketID (division).
type ImportRosterRequest struct {
	BracketID string               `json:"bracketId"`
	Players   []ImportRosterPlayer `json:"players"`
}

// ImportRosterPlayer is one row of an imported roster.
type ImportRosterPlayer struct {
	FullName string `json:"fullName"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
}

// ImportRosterResult summarizes a bulk roster import.
type ImportRosterResult struct {
	Added   int      `json:"added"`
	Skipped int      `json:"skipped"` // already registered (deduped)
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

// ImportDuprRequest imports a DUPR club's roster into an event. DuprClubID empty
// -> the platform's configured DUPR club.
type ImportDuprRequest struct {
	BracketID  string `json:"bracketId"`
	DuprClubID string `json:"duprClubId"`
}

// Club is a first-class club/organization that owns events and has members.
type Club struct {
	ID          string `json:"id"`
	OwnerID     string `json:"ownerId"`
	Name        string `json:"name"`
	City        string `json:"city,omitempty"`
	Description string `json:"description,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
	DuprClubID  string `json:"duprClubId,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	// Per-view aggregates + caller flags (set when fetched for a specific user).
	MemberCount int  `json:"memberCount"`
	EventCount  int  `json:"eventCount"`
	IsOwner     bool `json:"isOwner"`
	IsMember    bool `json:"isMember"`
}

// CreateClubRequest is the create/edit payload for a club.
type CreateClubRequest struct {
	Name        string `json:"name"`
	City        string `json:"city"`
	Description string `json:"description"`
	DuprClubID  string `json:"duprClubId"`
}

// ClubStanding is one player's cumulative record ACROSS all of a club's events
// — the club's durable system-of-record leaderboard (all-time, by wins).
type ClubStanding struct {
	Name          string `json:"name"`
	Wins          int    `json:"wins"`
	Losses        int    `json:"losses"`
	GamesPlayed   int    `json:"gamesPlayed"`
	PointsFor     int    `json:"pointsFor"`
	PointsAgainst int    `json:"pointsAgainst"`
	PointDiff     int    `json:"pointDiff"`
	EventsPlayed  int    `json:"eventsPlayed"`
}

// ClubMember is one member of a club, with their display name + photo.
type ClubMember struct {
	UserID   string `json:"userId"`
	FullName string `json:"fullName,omitempty"`
	PhotoURL string `json:"photoUrl,omitempty"`
	Role     string `json:"role"`
}

// UserSearchResult is a followable account in player search / followers / following
// lists: display name + DUPR rating from their player row, photo from pmp_profiles,
// and whether the calling user already follows them.
type UserSearchResult struct {
	UserID        string   `json:"userId"`
	FullName      string   `json:"fullName,omitempty"`
	PhotoURL      string   `json:"photoUrl,omitempty"`
	DoublesRating *float64 `json:"doublesRating,omitempty"`
	IsFollowing   bool     `json:"isFollowing"`
}

// RegistrationDetailsRequest edits a registered player's details (organizer-only).
// Writes the shared players row behind the registration.
type RegistrationDetailsRequest struct {
	FullName   string   `json:"fullName"`
	DuprRating *float64 `json:"duprRating"`
}

// GameScore is one game's result within a match. A best-of-1 match has a single
// game; a best-of-3 match has 2 or 3.
type GameScore struct {
	Team1 int `json:"team1"`
	Team2 int `json:"team2"`
}

// ScoreRequest records a match result. Games is the per-game scores for a
// best-of-N match; Team1Score/Team2Score are the legacy single-game fields,
// accepted when Games is empty (treated as one game).
type ScoreRequest struct {
	Team1Score int         `json:"team1Score"`
	Team2Score int         `json:"team2Score"`
	Games      []GameScore `json:"games,omitempty"`
}

// ForfeitRequest resolves a match without a fully-played score (no-show /
// retire / walkover). WinningTeam is the team credited the win. For a
// retirement the partial score at the stoppage may be supplied — it is kept as
// the real result and counts toward point differential. Forfeits/walkovers
// ignore the scores (a conventional win is recorded and excluded from diff).
type ForfeitRequest struct {
	WinningTeam int    `json:"winningTeam"`
	Kind        string `json:"kind"` // forfeit | retire | walkover
	Team1Score  *int   `json:"team1Score,omitempty"`
	Team2Score  *int   `json:"team2Score,omitempty"`
}

type PayRequest struct {
	Provider string `json:"provider"` // stripe | paypal | venmo | manual
	// Token is the registration's check_in_token, proving the caller owns this
	// registration (alternative to the X-Registration-Token header or event-owner JWT).
	Token string `json:"token"`
}

type CheckinRequest struct {
	Method string `json:"method"` // manual | qr | code
}

type TokenRequest struct {
	Token string `json:"token"`
}

type PhoneCheckinRequest struct {
	Phone string `json:"phone"`
}

// SwapRequest replaces one player in a match with another (player IDs).
type SwapRequest struct {
	OutPlayerID string `json:"outPlayerId"`
	InPlayerID  string `json:"inPlayerId"`
}

// SwapCrossRequest exchanges two players who are each in a DIFFERENT match:
// PlayerA (in MatchA) trades places with PlayerB (in MatchB). Team slots are
// preserved on each side. Used by drag-a-player-onto-another in the schedule.
type SwapCrossRequest struct {
	MatchA  string `json:"matchA"`
	PlayerA string `json:"playerA"`
	MatchB  string `json:"matchB"`
	PlayerB string `json:"playerB"`
}

// SetCourtRequest reassigns a match's court and/or its within-court play order.
// PlayOrder set => use it; nil with a court => append to the end of that court's
// queue; CourtNumber <= 0 => clear the court and the play order.
type SetCourtRequest struct {
	CourtNumber int      `json:"courtNumber"`
	PlayOrder   *float64 `json:"playOrder,omitempty"`
}

type PasscodeRequest struct {
	Code string `json:"code"`
}

type ShirtOrder struct {
	ID             string `json:"id"`
	RegistrationID string `json:"registrationId"`
	Size           string `json:"size"`
	NameOnShirt    string `json:"nameOnShirt,omitempty"`
	Number         string `json:"number,omitempty"`
	Color          string `json:"color,omitempty"`
	Status         string `json:"status"`
}

// AddonsRequest sets a registrant's paid add-on choices (token- or owner-gated).
type AddonsRequest struct {
	Token   string `json:"token"`
	Tee     bool   `json:"tee"`
	Grips   bool   `json:"grips"`
	TeeSize string `json:"teeSize,omitempty"` // required when Tee and the event offers sizes
}

// TeeOrder is one registrant's event-tee purchase (organizer fulfillment list).
type TeeOrder struct {
	RegistrationID string `json:"registrationId"`
	PlayerName     string `json:"playerName"`
	Size           string `json:"size"`
	Paid           bool   `json:"paid"`
}

// TeeOrdersSummary rolls up an event's tee orders for the printer: the design,
// a size→count breakdown, and the per-registrant list.
type TeeOrdersSummary struct {
	Name       string         `json:"name"`
	PriceCents int            `json:"priceCents"`
	Currency   string         `json:"currency"`
	FrontURL   string         `json:"frontUrl,omitempty"`
	BackURL    string         `json:"backUrl,omitempty"`
	Total      int            `json:"total"`
	SizeCounts map[string]int `json:"sizeCounts"`
	Orders     []TeeOrder     `json:"orders"`
}

type ShirtRequest struct {
	Size        string `json:"size"`
	NameOnShirt string `json:"nameOnShirt"`
	Number      string `json:"number"`
	Color       string `json:"color"`
	// Token is the registration's check_in_token, proving the caller owns this
	// registration (alternative to the X-Registration-Token header or event-owner JWT).
	Token string `json:"token"`
}

// FeedItem is one entry in a tournament's activity feed (auto activity or an
// organizer announcement, by Type).
type FeedItem struct {
	ID        string  `json:"id"`
	EventID   string  `json:"eventId"`
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	ActorName *string `json:"actorName,omitempty"`
	// ActorPhoto is the author's profile photo URL (community/user posts), attached
	// when listing the feed so a real post shows the poster's face. AuthorID is the
	// author's account id, used only server-side to look the photo up (not serialized).
	ActorPhoto *string `json:"actorPhoto,omitempty"`
	AuthorID   string  `json:"-"`
	RefID      *string `json:"refId,omitempty"`
	CreatedAt  string  `json:"createdAt"`
	// PosterURL + StartsAt are set only on `event`-type posts (an event that
	// announced itself to the NewsFeed) — sourced from the item's meta JSON so
	// the card can show the poster + date without a second fetch.
	PosterURL *string `json:"posterUrl,omitempty"`
	StartsAt  *string `json:"startsAt,omitempty"`
	// MediaURL + MediaType attach an uploaded photo/video to an announcement post
	// (also stashed in the item's meta JSON). MediaType is "video" or "image".
	MediaURL  *string `json:"mediaUrl,omitempty"`
	MediaType *string `json:"mediaType,omitempty"`
	// EventName is the parent event's name. Attached only by MyFeed (the app's
	// NewsFeed aggregates activity across many events and needs the label);
	// empty on the per-event feed where the event is already in context.
	EventName string `json:"eventName,omitempty"`
	// Social rollups (filled by ListFeed). ReactionCounts maps reaction type ->
	// count; MyReactions are the types the calling user reacted with (empty when
	// anonymous); CommentCount is the number of comments.
	ReactionCounts map[string]int `json:"reactionCounts"`
	MyReactions    []string       `json:"myReactions"`
	CommentCount   int            `json:"commentCount"`
}

// FeedPostRequest is an organizer announcement posted to the feed.
// RecurringControlsRequest updates a perpetual league's schedule. Any nil field
// is left unchanged; SkipUntil="" clears the skip.
type RecurringControlsRequest struct {
	StartsAt  *string `json:"startsAt,omitempty"`  // RFC3339 — reschedule weekday/time
	Paused    *bool   `json:"paused,omitempty"`    // pause/resume the league
	SkipUntil *string `json:"skipUntil,omitempty"` // YYYY-MM-DD — skip up to this date ("" clears)
}

type FeedPostRequest struct {
	Text string `json:"text"`
	// Announcement: mark this as an official ANNOUNCEMENT. Only honored for the
	// event owner (organizer) — a player's post is always a regular post. An
	// announcement is chip-marked and reads as the organizer's official voice.
	Announcement bool `json:"announcement"`
	// Notify is deprecated — every feed post now pushes + bells the event
	// audience (players + owner) except the poster. Kept for old clients.
	Notify bool `json:"notify"`
	// MediaURL + MediaType attach an uploaded video (or image) to the post. A
	// post may be media-only (empty Text). MediaType is "video" or "image".
	MediaURL  string `json:"mediaUrl"`
	MediaType string `json:"mediaType"`
}

// ReactionRequest toggles a reaction of Type on a feed item.
type ReactionRequest struct {
	Type string `json:"type"` // like | love | fire
}

// ReactionResult is the new state after a toggle.
type ReactionResult struct {
	Reacted bool           `json:"reacted"` // is the caller now reacting with Type
	Counts  map[string]int `json:"counts"`
}

// FeedComment is one comment on a feed item.
// UserNotification is one entry in a user's in-app activity feed (the bell):
// someone followed them, reacted to / commented on their post, or registered
// for their event. Type drives the client's icon + copy; Link is a deep-link
// target the client routes on tap (e.g. "event:<id>", "profile:<id>", "feed").
type UserNotification struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	ActorID   string `json:"actorId,omitempty"`
	ActorName string `json:"actorName"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Link      string `json:"link"`
	Read      bool   `json:"read"`
	CreatedAt string `json:"createdAt"`
}

type FeedComment struct {
	ID         string `json:"id"`
	FeedItemID string `json:"feedItemId"`
	AuthorName string `json:"authorName"`
	Text       string `json:"text"`
	Mine       bool   `json:"mine"` // authored by the calling user
	CanDelete  bool   `json:"canDelete"`
	CreatedAt  string `json:"createdAt"`
	// ReportCount is populated ONLY for the event owner (moderation view) so they
	// can see and act on flagged comments; 0/omitted for everyone else.
	ReportCount int `json:"reportCount,omitempty"`
}

// BlockedUser is one account the caller has blocked (for the "Blocked accounts"
// management list). Name is best-effort display only.
type BlockedUser struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
}

// CommentRequest adds a comment to a feed item.
type CommentRequest struct {
	Text string `json:"text"`
}

// ---- Stripe Connect (real online payments) ----

// StripeConnectRequest starts/resumes an organizer's Stripe Connect onboarding.
// returnUrl is where Stripe sends the organizer when onboarding completes;
// refreshUrl when the (one-time) link expires. Both are required.
type StripeConnectRequest struct {
	ReturnURL  string `json:"returnUrl"`
	RefreshURL string `json:"refreshUrl"`
}

// URLResponse carries a single Stripe-hosted URL (onboarding link or Checkout
// session) for the client to redirect to.
type URLResponse struct {
	URL string `json:"url"`
}

// StripeStatusResponse reports an organizer's Stripe Connect onboarding state.
type StripeStatusResponse struct {
	Connected      bool `json:"connected"`
	ChargesEnabled bool `json:"chargesEnabled"`
}

// CheckoutRequest starts a Stripe Checkout Session for a registration's entry
// fee. token proves ownership of the registration (the check_in_token) for the
// public/registrant path, mirroring PayRequest. successUrl/cancelUrl are where
// Stripe returns the payer after paying or cancelling.
type CheckoutRequest struct {
	Token      string `json:"token,omitempty"`
	SuccessURL string `json:"successUrl"`
	CancelURL  string `json:"cancelUrl"`
}

// RosterEntry is one player in an event's PUBLIC roster — name, division, and
// check-in status only (NO phone/email/DUPR), safe to show players/spectators.
// PlayerID links to the public player profile (it's an opaque id, not PII).
type RosterEntry struct {
	PlayerID string `json:"playerId,omitempty"`
	FullName string `json:"fullName"`
	Division string `json:"division,omitempty"`
	// PartnerID is the doubles partner's player id (empty for singles / unpaired),
	// so the player list can group a pair (their "team") together.
	PartnerID string `json:"partnerId,omitempty"`
	CheckedIn bool   `json:"checkedIn"`
	// PhotoURL is the linked account's profile photo (pmp_profiles); empty for
	// name-only players — the UI falls back to initials.
	PhotoURL string `json:"photoUrl,omitempty"`
}

// PlayerProfile is a PUBLIC player page: name + DUPR id/ratings (when the player
// has connected DUPR) and their across-events box score (wins/losses, points,
// tournaments played). No contact PII — safe for spectators/opponents.
type PlayerProfile struct {
	PlayerID      string   `json:"playerId"`
	FullName      string   `json:"fullName"`
	PhotoURL      string   `json:"photoUrl,omitempty"`
	DuprID        string   `json:"duprId,omitempty"`
	DoublesRating *float64 `json:"doublesRating,omitempty"`
	SinglesRating *float64 `json:"singlesRating,omitempty"`
	EventsPlayed  int      `json:"eventsPlayed"`
	Wins          int      `json:"wins"`
	Losses        int      `json:"losses"`
	GamesPlayed   int      `json:"gamesPlayed"`
	PointsFor     int      `json:"pointsFor"`
	PointsAgainst int      `json:"pointsAgainst"`
	RecentEvents  []string `json:"recentEvents"`
	// Kudos — always-positive peer recognition this player has received, tallied
	// by label. KudosGivers is the distinct-giver count (the anti-spam Street
	// Cred signal); CanReceiveKudos is true when this player is linked to an
	// account (only linked players can be recognized).
	Kudos           []KudosTally `json:"kudos"`
	KudosGivers     int          `json:"kudosGivers"`
	CanReceiveKudos bool         `json:"canReceiveKudos"`
	// IsSelf is true when the (optionally-authenticated) caller IS this player's
	// account — the client uses it to hide "Give kudos" on your own profile. We
	// return a boolean rather than the raw account id so the PUBLIC profile
	// endpoint never leaks an enumerable account UUID to anonymous callers.
	IsSelf bool `json:"isSelf"`
}

// KudosTally is a count of one kind of peer recognition a player has received.
type KudosTally struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Profile is the signed-in user's saved player details, used to pre-fill the
// registration form. Email always reflects the verified token.
type Profile struct {
	FullName   string   `json:"fullName"`
	Phone      string   `json:"phone"`
	Email      string   `json:"email"`
	DuprID     string   `json:"duprId"`
	DuprRating *float64 `json:"duprRating,omitempty"`
	SkillLevel *float64 `json:"skillLevel,omitempty"`
	// GamesPlayed: completed real matches across all the user's player rows.
	GamesPlayed int    `json:"gamesPlayed"`
	PhotoURL    string `json:"photoUrl,omitempty"`
	// Partner-finder fields (account-level, pmp_profiles).
	Gender         string `json:"gender,omitempty"`
	City           string `json:"city,omitempty"`
	SeekingPartner bool   `json:"seekingPartner"`
	// Onboarded: the one-time first-run questionnaire has been shown to this
	// account. Stored server-side (pmp_profiles) so it survives a cache clear / a
	// new device, instead of only a device-local flag.
	Onboarded bool `json:"onboarded"`
}

// ProfileDetailsRequest saves the caller's partner-finder fields.
type ProfileDetailsRequest struct {
	Gender         string `json:"gender"`
	City           string `json:"city"`
	SeekingPartner bool   `json:"seekingPartner"`
}

// BasicInfoRequest saves the caller's account-level name + phone.
type BasicInfoRequest struct {
	FullName string `json:"fullName"`
	Phone    string `json:"phone"`
}

// PartnerResult is one find-a-partner directory row.
type PartnerResult struct {
	UserSearchResult
	Gender string `json:"gender,omitempty"`
	City   string `json:"city,omitempty"`
}

// Vendor is one Vendor Village entry on an event — a booth, food truck, or
// sponsor the organizer wants attendees to see (and optionally get a push
// about). Managed by the organizer; listed publicly on the event views.
type Vendor struct {
	ID        string `json:"id"`
	EventID   string `json:"eventId"`
	Name      string `json:"name"`
	Tagline   string `json:"tagline,omitempty"`
	Booth     string `json:"booth,omitempty"`
	Promo     string `json:"promo,omitempty"`
	LinkURL   string `json:"linkUrl,omitempty"`
	LogoURL   string `json:"logoUrl,omitempty"`
	SortOrder int    `json:"sortOrder"`
	// Status: pending (public application awaiting review) | approved (shown
	// on the public strip) | rejected. Organizer-added vendors are approved.
	Status string `json:"status"`
	// Applicant contact details — owner-only; stripped from public reads.
	ContactEmail string `json:"contactEmail,omitempty"`
	ContactPhone string `json:"contactPhone,omitempty"`
	Pitch        string `json:"pitch,omitempty"`
	// Booth fee (organizer-set) + its payment state. PayToken gates the public
	// pay page (vendors have no accounts) — owner-only, stripped publicly.
	FeeCents      int    `json:"feeCents"`
	PaymentStatus string `json:"paymentStatus,omitempty"`
	PayToken      string `json:"payToken,omitempty"`
	// SponsorCourt: this vendor "presents" court N ("Court 3 · presented by X"
	// on the board + court calls); 0 = none. Clicks counts card tap-throughs.
	SponsorCourt int `json:"sponsorCourt"`
	// IsSponsor marks a paid SPONSOR SLOT (organizer-sold): same Village rails,
	// but its payment carries a flat 10% uncapped platform fee (B2B).
	IsSponsor bool `json:"isSponsor,omitempty"`
	Clicks    int  `json:"clicks"`
}

// VendorApplyRequest is the public "Become a vendor" application form.
type VendorApplyRequest struct {
	Name         string `json:"name"`
	Tagline      string `json:"tagline"`
	Pitch        string `json:"pitch"`
	LinkURL      string `json:"linkUrl"`
	ContactEmail string `json:"contactEmail"`
	ContactPhone string `json:"contactPhone"`
	CaptchaToken string `json:"captchaToken"`
}

// VendorRequest carries an organizer's create/update of a Vendor Village entry.
type VendorRequest struct {
	Name         string `json:"name"`
	Tagline      string `json:"tagline"`
	Booth        string `json:"booth"`
	Promo        string `json:"promo"`
	LinkURL      string `json:"linkUrl"`
	LogoURL      string `json:"logoUrl"`
	SortOrder    int    `json:"sortOrder"`
	FeeCents     int    `json:"feeCents"`
	SponsorCourt int    `json:"sponsorCourt"`
	IsSponsor    bool   `json:"isSponsor"`
}

// VendorNotifyRequest is the organizer-composed push about a vendor deal.
type VendorNotifyRequest struct {
	Message string `json:"message"`
}

// BlockedContact is one entry on the platform denylist — a phone (normalized
// digits) and/or email blocked from registering anywhere on PlanMyPickle.
type BlockedContact struct {
	ID        string `json:"id"`
	Phone     string `json:"phone,omitempty"`
	Email     string `json:"email,omitempty"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// BlockContactRequest is the owner's add-to-denylist payload (phone and/or email).
type BlockContactRequest struct {
	Phone  string `json:"phone"`
	Email  string `json:"email"`
	Reason string `json:"reason"`
}

// CoachApplication is a coach asking to teach on the platform.
//
// Separate from Instructor: an application is a REQUEST, an instructor row is
// ACCESS. Approving turns one into the other, and keeping them apart means the
// allowlist never gains a row nobody decided on.
type CoachApplication struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Phone string `json:"phone,omitempty"`
	City  string `json:"city,omitempty"`
	// Free text on purpose — a cert number, a club, a coaching history. A rigid
	// schema would reject the honest applicant with an unusual answer.
	Certifications string `json:"certifications,omitempty"`
	Experience     string `json:"experience,omitempty"`
	HasInsurance   bool   `json:"hasInsurance"`
	Note           string `json:"note,omitempty"`
	Status         string `json:"status"` // pending | approved | rejected
	DecisionNote   string `json:"decisionNote,omitempty"`
	CreatedAt      string `json:"createdAt,omitempty"`
}
