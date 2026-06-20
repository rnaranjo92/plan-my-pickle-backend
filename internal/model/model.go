// Package model holds the API/domain structs shared by the service and HTTP layers.
package model

type Event struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Format               string   `json:"format"`           // singles | doubles
	PartnerMode          string   `json:"partnerMode"`      // fixed | rotating | na
	TournamentFormat     string   `json:"tournamentFormat"` // round_robin | single_elim | pools_playoff
	ScoringMode          string   `json:"scoringMode"`      // points | wins
	NumCourts            int      `json:"numCourts"`
	PointsToWin          int      `json:"pointsToWin"`
	WinBy                int      `json:"winBy"`
	BestOf               int      `json:"bestOf"` // games per match: 1 (single) or 3 (best of 3)
	GameDurationMinutes  int      `json:"gameDurationMinutes"`
	RegistrationFeeCents int      `json:"registrationFeeCents"`
	Currency             string   `json:"currency"`
	Location             *string  `json:"location,omitempty"`
	ContactPhone         *string  `json:"contactPhone,omitempty"`
	VenueName            *string  `json:"venueName,omitempty"`
	VenueAddress         *string  `json:"venueAddress,omitempty"`
	VenuePhone           *string  `json:"venuePhone,omitempty"`
	VenueWebsite         *string  `json:"venueWebsite,omitempty"`
	VenueLat             *float64 `json:"venueLat,omitempty"`
	VenueLng             *float64 `json:"venueLng,omitempty"`
	DuprSanctioned       bool     `json:"duprSanctioned"`
	// Consolation enables a consolation back-draw for single_elim (first-round
	// losers play down to a consolation champion / bronze — USAP 12.J ≥2 matches).
	Consolation bool `json:"consolation"`
	// StartsAt is the scheduled tournament start (RFC3339 UTC), or nil.
	StartsAt *string `json:"startsAt,omitempty"`
	// EndsAt is the scheduled end (RFC3339 UTC), or nil — for multi-day events.
	EndsAt      *string `json:"endsAt,omitempty"`
	Description *string `json:"description,omitempty"`
	// RegisteredCount is the number of players registered (filled on the
	// dashboard list; 0 on single-event reads).
	RegisteredCount int `json:"registeredCount"`
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
}

type Registration struct {
	ID            string   `json:"id"`
	EventID       string   `json:"eventId"`
	PlayerID      string   `json:"playerId"`
	FullName      string   `json:"fullName"`
	BracketID     *string  `json:"bracketId,omitempty"`
	PaymentStatus string   `json:"paymentStatus"`
	CheckedIn     bool     `json:"checkedIn"`
	CheckInToken  *string  `json:"checkInToken,omitempty"`
	Phone         string   `json:"phone"`
	DuprID        *string  `json:"duprId,omitempty"`
	DuprRating    *float64 `json:"duprRating,omitempty"`
	// OutsideRating is true when the player's DUPR rating falls outside their
	// chosen division's rating band (a soft flag, not a block).
	OutsideRating bool `json:"outsideRating"`
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

type Match struct {
	ID        string  `json:"id"`
	BracketID *string `json:"bracketId,omitempty"`
	Stage     string  `json:"stage"` // pool | bracket
	// BracketTier classifies a bracket match for rendering: main | consolation |
	// winners | losers | grand_final. Empty/"main" for ordinary brackets.
	BracketTier     string   `json:"bracketTier,omitempty"`
	BracketRound    *int     `json:"bracketRound,omitempty"`
	BracketSlot     *int     `json:"bracketSlot,omitempty"`
	CourtNumber     *int     `json:"courtNumber,omitempty"`
	PlayOrder       *float64 `json:"playOrder,omitempty"`       // within-court order, lower first
	DurationMinutes *int     `json:"durationMinutes,omitempty"` // per-match length override
	ScheduledDay    *int     `json:"scheduledDay,omitempty"`    // 0-based tournament day; null = auto-split
	Team1Score      *int     `json:"team1Score,omitempty"`      // total points across all games
	Team2Score      *int     `json:"team2Score,omitempty"`
	WinningTeam     *int     `json:"winningTeam,omitempty"` // series winner (games won)
	// Games is the per-game breakdown for a best-of-N match (omitted for legacy
	// single-game matches scored before per-game tracking).
	Games      []GameScore `json:"games,omitempty"`
	Status     string      `json:"status"`
	ResultType string      `json:"resultType,omitempty"` // normal | forfeit | retire | walkover
	// Round context — populated by the event-wide pool-matches query so the
	// Game tab can group + filter every match from one stream.
	RoundID     *string `json:"roundId,omitempty"`
	RoundNumber *int    `json:"roundNumber,omitempty"`
	RoundStatus string  `json:"roundStatus,omitempty"`
	Sides       []Side  `json:"sides"`
}

type RoundView struct {
	ID          string  `json:"id"`
	BracketID   *string `json:"bracketId,omitempty"`
	RoundNumber int     `json:"roundNumber"`
	Status      string  `json:"status"`
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
}

// ---- request DTOs ----

type BracketInput struct {
	Name      string   `json:"name"`
	MinRating *float64 `json:"minRating,omitempty"`
	MaxRating *float64 `json:"maxRating,omitempty"`
	MinAge    *int     `json:"minAge,omitempty"`
	MaxAge    *int     `json:"maxAge,omitempty"`
}

type CreateEventRequest struct {
	Name                 string         `json:"name"`
	Format               string         `json:"format"`           // singles|doubles (default doubles)
	PartnerMode          string         `json:"partnerMode"`      // fixed|rotating (default rotating)
	TournamentFormat     string         `json:"tournamentFormat"` // default round_robin
	ScoringMode          string         `json:"scoringMode"`      // default wins
	NumCourts            int            `json:"numCourts"`
	PointsToWin          int            `json:"pointsToWin"`
	WinBy                int            `json:"winBy"`
	BestOf               int            `json:"bestOf"`
	GameDurationMinutes  int            `json:"gameDurationMinutes"`
	RegistrationFeeCents int            `json:"registrationFeeCents"`
	Location             string         `json:"location"`
	ContactPhone         string         `json:"contactPhone"`
	VenueName            string         `json:"venueName"`
	VenueAddress         string         `json:"venueAddress"`
	VenuePhone           string         `json:"venuePhone"`
	VenueWebsite         string         `json:"venueWebsite"`
	VenueLat             *float64       `json:"venueLat"`
	VenueLng             *float64       `json:"venueLng"`
	DuprSanctioned       bool           `json:"duprSanctioned"`
	Consolation          bool           `json:"consolation"` // single_elim back-draw
	StartsAt             string         `json:"startsAt"`    // RFC3339 UTC, "" = none
	EndsAt               string         `json:"endsAt"`      // RFC3339 UTC, "" = none
	Description          string         `json:"description"`
	AdminPasscode        string         `json:"adminPasscode"`
	Brackets             []BracketInput `json:"brackets"`
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
	BracketID       string   `json:"bracketId"`
	// Self is true only when a LOGGED-IN user is registering THEMSELVES (the
	// self-registration flow). It links the player to their account
	// (players.user_id); an organizer adding other players leaves it false.
	Self bool `json:"self"`
	// CaptchaToken is a Cloudflare Turnstile token sent only by the PUBLIC
	// self-registration form (anonymous). The handler verifies it server-side;
	// the service ignores it.
	CaptchaToken string `json:"captchaToken,omitempty"`
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

type ShirtRequest struct {
	Size        string `json:"size"`
	NameOnShirt string `json:"nameOnShirt"`
	Number      string `json:"number"`
	Color       string `json:"color"`
}

// FeedItem is one entry in a tournament's activity feed (auto activity or an
// organizer announcement, by Type).
type FeedItem struct {
	ID        string  `json:"id"`
	EventID   string  `json:"eventId"`
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	ActorName *string `json:"actorName,omitempty"`
	RefID     *string `json:"refId,omitempty"`
	CreatedAt string  `json:"createdAt"`
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
type FeedPostRequest struct {
	Text string `json:"text"`
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
type FeedComment struct {
	ID         string `json:"id"`
	FeedItemID string `json:"feedItemId"`
	AuthorName string `json:"authorName"`
	Text       string `json:"text"`
	Mine       bool   `json:"mine"` // authored by the calling user
	CanDelete  bool   `json:"canDelete"`
	CreatedAt  string `json:"createdAt"`
}

// CommentRequest adds a comment to a feed item.
type CommentRequest struct {
	Text string `json:"text"`
}

// RosterEntry is one player in an event's PUBLIC roster — name, division, and
// check-in status only (NO phone/email/DUPR), safe to show players/spectators.
type RosterEntry struct {
	FullName  string `json:"fullName"`
	Division  string `json:"division,omitempty"`
	CheckedIn bool   `json:"checkedIn"`
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
}
