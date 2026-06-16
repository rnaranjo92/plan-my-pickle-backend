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
	RegistrationFeeCents int      `json:"registrationFeeCents"`
	Currency             string   `json:"currency"`
	Location             *string  `json:"location,omitempty"`
	VenueName            *string  `json:"venueName,omitempty"`
	VenueAddress         *string  `json:"venueAddress,omitempty"`
	VenuePhone           *string  `json:"venuePhone,omitempty"`
	VenueWebsite         *string  `json:"venueWebsite,omitempty"`
	VenueLat             *float64 `json:"venueLat,omitempty"`
	VenueLng             *float64 `json:"venueLng,omitempty"`
	DuprSanctioned       bool     `json:"duprSanctioned"`
	Status               string   `json:"status"`
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
	ID           string  `json:"id"`
	BracketID    *string `json:"bracketId,omitempty"`
	Stage        string  `json:"stage"` // pool | bracket
	BracketRound *int    `json:"bracketRound,omitempty"`
	BracketSlot  *int    `json:"bracketSlot,omitempty"`
	CourtNumber  *int    `json:"courtNumber,omitempty"`
	Team1Score   *int    `json:"team1Score,omitempty"`
	Team2Score   *int    `json:"team2Score,omitempty"`
	WinningTeam  *int    `json:"winningTeam,omitempty"`
	Status       string  `json:"status"`
	ResultType   string  `json:"resultType,omitempty"` // normal | forfeit | retire | walkover
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
	RegistrationFeeCents int            `json:"registrationFeeCents"`
	Location             string         `json:"location"`
	VenueName            string         `json:"venueName"`
	VenueAddress         string         `json:"venueAddress"`
	VenuePhone           string         `json:"venuePhone"`
	VenueWebsite         string         `json:"venueWebsite"`
	VenueLat             *float64       `json:"venueLat"`
	VenueLng             *float64       `json:"venueLng"`
	DuprSanctioned       bool           `json:"duprSanctioned"`
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
}

type ScoreRequest struct {
	Team1Score int `json:"team1Score"`
	Team2Score int `json:"team2Score"`
}

// ForfeitRequest resolves a match without a played score (no-show / retire /
// walkover). WinningTeam is the team credited the win.
type ForfeitRequest struct {
	WinningTeam int    `json:"winningTeam"`
	Kind        string `json:"kind"` // forfeit | retire | walkover
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
