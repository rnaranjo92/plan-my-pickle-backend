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
	VenueNotes           *string  `json:"venueNotes,omitempty"`
	WaiverURL            *string  `json:"waiverUrl,omitempty"`
	VenueName            *string  `json:"venueName,omitempty"`
	VenueAddress         *string  `json:"venueAddress,omitempty"`
	VenuePhone           *string  `json:"venuePhone,omitempty"`
	VenueWebsite         *string  `json:"venueWebsite,omitempty"`
	VenueLat             *float64 `json:"venueLat,omitempty"`
	VenueLng             *float64 `json:"venueLng,omitempty"`
	DuprSanctioned       bool     `json:"duprSanctioned"`
	// CashPrize flags a cash-prize event; CashPrizeAmount is the optional pot size.
	CashPrize       bool     `json:"cashPrize"`
	CashPrizeAmount *float64 `json:"cashPrizeAmount,omitempty"`
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
	// Listed = organizer opted this event into the public "Nearby" discovery feed.
	Listed bool `json:"listed"`
	// PosterURL is the uploaded event poster (public Storage URL), or nil.
	PosterURL *string `json:"posterUrl,omitempty"`
	// DistanceKm is set only in Nearby results — km from the requester.
	DistanceKm *float64 `json:"distanceKm,omitempty"`
	// ScheduleBreaks are organizer-defined blocked time ranges (e.g. lunch) the
	// schedule timeline skips. Minutes from midnight; applied to each day.
	ScheduleBreaks []ScheduleBreak `json:"scheduleBreaks"`
	// DayCapMinutes: if set, no games start past this time-of-day; the rest roll
	// to the next tournament day. Minutes from midnight; nil = no cap.
	DayCapMinutes *int `json:"dayCapMinutes,omitempty"`
	// LeagueID links this event to a league (season/recurring play) it belongs to,
	// or nil for a standalone event.
	LeagueID *string `json:"leagueId,omitempty"`
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
	// Sanctioned flags an officially sanctioned league.
	Sanctioned bool `json:"sanctioned"`
	// CashPrize flags a cash-prize league; CashPrizeAmount is the optional pot.
	CashPrize       bool     `json:"cashPrize"`
	CashPrizeAmount *float64 `json:"cashPrizeAmount,omitempty"`
	// FirstSessionAt / LastSessionAt are the earliest start and latest end (or
	// start) across the league's sessions (events), RFC3339 UTC — populated on
	// the MyLeagues list so the home screen can group leagues by lifecycle
	// (Happening now / Upcoming / Past). Nil when the league has no dated session.
	FirstSessionAt *string `json:"firstSessionAt,omitempty"`
	LastSessionAt  *string `json:"lastSessionAt,omitempty"`
}

// CreateLeagueRequest is the create-payload for a league.
type CreateLeagueRequest struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	LeagueType      string   `json:"leagueType"` // round_robin | ladder | team (default round_robin)
	DayType         string   `json:"dayType"`    // single | multi (default multi)
	Sanctioned      bool     `json:"sanctioned"`
	CashPrize       bool     `json:"cashPrize"`
	CashPrizeAmount *float64 `json:"cashPrizeAmount,omitempty"`
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
	Score           string `json:"score"`
	// PlayedAt is an optional ISO-8601 timestamp; empty defaults to now().
	PlayedAt string `json:"playedAt"`
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
	// Partner pairing (doubles). PartnerID is the partner's PLAYER id when paired
	// with a registered player (set mutually on both registrations); PartnerName
	// is that partner's resolved display name. PartnerNote holds a free-text
	// partner name when the partner isn't a registered player. All nil for an
	// unpaired or singles registration.
	PartnerID   *string `json:"partnerId,omitempty"`
	PartnerName *string `json:"partnerName,omitempty"`
	PartnerNote *string `json:"partnerNote,omitempty"`
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
	VenueNotes           string         `json:"venueNotes"`
	WaiverURL            string         `json:"waiverUrl"`
	VenueName            string         `json:"venueName"`
	VenueAddress         string         `json:"venueAddress"`
	VenuePhone           string         `json:"venuePhone"`
	VenueWebsite         string         `json:"venueWebsite"`
	VenueLat             *float64       `json:"venueLat"`
	VenueLng             *float64       `json:"venueLng"`
	DuprSanctioned       bool           `json:"duprSanctioned"`
	CashPrize            bool           `json:"cashPrize"`
	CashPrizeAmount      *float64       `json:"cashPrizeAmount,omitempty"`
	Consolation          bool           `json:"consolation"` // single_elim back-draw
	StartsAt             string         `json:"startsAt"`    // RFC3339 UTC, "" = none
	EndsAt               string         `json:"endsAt"`      // RFC3339 UTC, "" = none
	Description          string         `json:"description"`
	AdminPasscode        string         `json:"adminPasscode"`
	Brackets             []BracketInput `json:"brackets"`
	Listed               bool           `json:"listed"`
	PosterURL            string         `json:"posterUrl"`
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
