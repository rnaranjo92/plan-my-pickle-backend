package service

import (
	"strconv"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// PostgREST returns rows as map[string]any: text -> string, int/numeric ->
// float64, boolean -> bool, uuid -> string, timestamptz -> RFC3339 string, null
// -> nil (or key absent). These helpers extract typed fields from those maps.

func asStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func asStrPtr(m map[string]any, k string) *string {
	if v, ok := m[k].(string); ok && v != "" {
		return &v
	}
	return nil
}

func asInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func asIntPtr(m map[string]any, k string) *int {
	switch v := m[k].(type) {
	case float64:
		n := int(v)
		return &n
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return &n
		}
	}
	return nil
}

func asBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func asFloatPtr(m map[string]any, k string) *float64 {
	switch v := m[k].(type) {
	case float64:
		return &v
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return &f
		}
	}
	return nil
}

// orNull maps an empty string to a JSON null (so PostgREST stores NULL), else
// the string itself. Use for nullable text/uuid columns in Insert/Update bodies.
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// fOrNull maps a nil *float64 to JSON null, else the dereferenced value.
func fOrNull(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// iOrNull maps a nil *int to JSON null, else the dereferenced value.
func iOrNull(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// intOr dereferences a *int, returning def when nil — used as a sort key so a
// missing value (e.g. unassigned court) sorts last.
func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// strOr dereferences a *string, returning "" when nil.
func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// asMap returns an embedded object (PostgREST resource embedding). PostgREST
// returns a to-one embed as an object, or sometimes a one-element array.
func asMap(m map[string]any, k string) map[string]any {
	switch v := m[k].(type) {
	case map[string]any:
		return v
	case []any:
		if len(v) > 0 {
			if mm, ok := v[0].(map[string]any); ok {
				return mm
			}
		}
	}
	return nil
}

// ---- row -> model mappers ----

func mapEvent(m map[string]any) model.Event {
	return model.Event{
		ID:                       asStr(m, "id"),
		Name:                     asStr(m, "name"),
		Format:                   asStr(m, "format"),
		PartnerMode:              asStr(m, "partner_mode"),
		TournamentFormat:         asStr(m, "tournament_format"),
		ScoringMode:              asStr(m, "scoring_mode"),
		NumCourts:                asInt(m, "num_courts"),
		PointsToWin:              asInt(m, "points_to_win"),
		WinBy:                    asInt(m, "win_by"),
		BestOf:                   asInt(m, "best_of"),
		GameDurationMinutes:      asInt(m, "game_duration_minutes"),
		MinPoolRounds:            asInt(m, "min_pool_rounds"),
		MaxPoolRounds:            asInt(m, "max_pool_rounds"),
		RegistrationFeeCents:     asInt(m, "registration_fee_cents"),
		Currency:                 asStr(m, "currency"),
		Location:                 asStrPtr(m, "location"),
		ContactPhone:             asStrPtr(m, "contact_phone"),
		ZelleHandle:              asStrPtr(m, "zelle_handle"),
		ClubID:                   asStrPtr(m, "club_id"),
		VenueNotes:               asStrPtr(m, "venue_notes"),
		WaiverURL:                asStrPtr(m, "waiver_url"),
		VenueName:                asStrPtr(m, "venue_name"),
		VenueAddress:             asStrPtr(m, "venue_address"),
		VenuePhone:               asStrPtr(m, "venue_phone"),
		VenueWebsite:             asStrPtr(m, "venue_website"),
		VenueLat:                 asFloatPtr(m, "venue_lat"),
		VenueLng:                 asFloatPtr(m, "venue_lng"),
		DuprSanctioned:           asBool(m, "dupr_sanctioned"),
		DuprMinEntitlement:       asStr(m, "dupr_min_entitlement"),
		CashPrize:                asBool(m, "cash_prize"),
		CashPrizeAmount:          asFloatPtr(m, "cash_prize_amount"),
		AddonTeeCents:            asInt(m, "addon_tee_cents"),
		AddonGripsCents:          asInt(m, "addon_grips_cents"),
		Consolation:              asBool(m, "consolation"),
		AutoAdjust:               asBool(m, "auto_adjust"),
		TeamSize:                 asInt(m, "team_size"),
		StartsAt:                 asStrPtr(m, "starts_at"),
		EndsAt:                   asStrPtr(m, "ends_at"),
		Listed:                   asBool(m, "listed"),
		PlayerScoring:            asBool(m, "player_scoring"),
		ScoreConfirmMinutes:      asInt(m, "score_confirm_minutes"),
		PosterURL:                asStrPtr(m, "poster_url"),
		SponsorWatermarkURL:      asStr(m, "sponsor_watermark_url"),
		SponsorWatermarkOpacity:  asFloatOr(m, "sponsor_watermark_opacity", 0.08),
		SponsorWatermarkPosition: asStr(m, "sponsor_watermark_position"),
		SponsorWatermarkScale:    asFloatOr(m, "sponsor_watermark_scale", 0.5),
		ScheduleBreaks:           mapBreaks(m),
		DayCapMinutes:            asIntPtr(m, "day_cap_minutes"),
		DayEndMinutes:            mapIntArray(m, "day_end_minutes"),
		Description:              asStrPtr(m, "description"),
		LeagueID:                 asStrPtr(m, "league_id"),
		Status:                   asStr(m, "status"),
		ScoreboardTheme:          asMap(m, "scoreboard_theme"),
	}
}

func mapLeague(m map[string]any) model.League {
	return model.League{
		ID:              asStr(m, "id"),
		OwnerID:         asStr(m, "owner_id"),
		Name:            asStr(m, "name"),
		Description:     asStrPtr(m, "description"),
		CreatedAt:       asStr(m, "created_at"),
		PosterURL:       asStrPtr(m, "poster_url"),
		LeagueType:      asStr(m, "league_type"),
		DayType:         asStr(m, "day_type"),
		Sanctioned:      asBool(m, "sanctioned"),
		CashPrize:       asBool(m, "cash_prize"),
		CashPrizeAmount: asFloatPtr(m, "cash_prize_amount"),
	}
}

// mapIntArray parses a jsonb int array (e.g. events.day_end_minutes). JSON
// numbers arrive as float64; positional, so callers keep the slot order.
func mapIntArray(m map[string]any, key string) []int {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(raw))
	for _, v := range raw {
		if f, ok := v.(float64); ok {
			out = append(out, int(f))
		} else {
			out = append(out, -1)
		}
	}
	return out
}

// mapBreaks parses the events.schedule_breaks jsonb array into typed breaks.
func mapBreaks(m map[string]any) []model.ScheduleBreak {
	raw, ok := m["schedule_breaks"].([]any)
	if !ok {
		return nil
	}
	out := make([]model.ScheduleBreak, 0, len(raw))
	for _, r := range raw {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, model.ScheduleBreak{
			StartMin: asInt(rm, "startMin"),
			EndMin:   asInt(rm, "endMin"),
			Label:    asStr(rm, "label"),
		})
	}
	return out
}

func mapBracket(m map[string]any) model.Bracket {
	return model.Bracket{
		ID:           asStr(m, "id"),
		EventID:      asStr(m, "event_id"),
		Name:         asStr(m, "name"),
		MinRating:    asFloatPtr(m, "min_rating"),
		MaxRating:    asFloatPtr(m, "max_rating"),
		MinAge:       asIntPtr(m, "min_age"),
		MaxAge:       asIntPtr(m, "max_age"),
		SortOrder:    asInt(m, "sort_order"),
		DivisionType: asStr(m, "division_type"),
		DuprMin:      asFloatPtr(m, "dupr_min"),
		DuprMax:      asFloatPtr(m, "dupr_max"),
	}
}

func mapLeagueBracket(m map[string]any) model.LeagueBracket {
	return model.LeagueBracket{
		ID:           asStr(m, "id"),
		LeagueID:     asStr(m, "league_id"),
		Name:         asStr(m, "name"),
		DivisionType: asStr(m, "division_type"),
		MinRating:    asFloatPtr(m, "min_rating"),
		MaxRating:    asFloatPtr(m, "max_rating"),
		MinAge:       asIntPtr(m, "min_age"),
		MaxAge:       asIntPtr(m, "max_age"),
		DuprMin:      asFloatPtr(m, "dupr_min"),
		DuprMax:      asFloatPtr(m, "dupr_max"),
		SortOrder:    asInt(m, "sort_order"),
	}
}

func mapLadderEntrant(m map[string]any) model.LadderEntrant {
	return model.LadderEntrant{
		ID:              asStr(m, "id"),
		LeagueBracketID: asStr(m, "league_bracket_id"),
		DisplayName:     asStr(m, "display_name"),
		PlayerID:        asStrPtr(m, "player_id"),
		IsTeam:          asBool(m, "is_team"),
		Position:        asInt(m, "position"),
	}
}

func mapLadderMatch(m map[string]any) model.LadderMatch {
	return model.LadderMatch{
		ID:              asStr(m, "id"),
		LeagueBracketID: asStr(m, "league_bracket_id"),
		EntrantAID:      asStr(m, "entrant_a_id"),
		EntrantBID:      asStr(m, "entrant_b_id"),
		WinnerEntrantID: asStr(m, "winner_entrant_id"),
		Score:           asStr(m, "score"),
		PlayedAt:        asStr(m, "played_at"),
	}
}

func mapTeam(m map[string]any) model.Team {
	return model.Team{
		ID:              asStr(m, "id"),
		LeagueBracketID: asStr(m, "league_bracket_id"),
		Name:            asStr(m, "name"),
		PlayerID:        asStrPtr(m, "player_id"),
	}
}

func mapTeamFixture(m map[string]any) model.TeamFixture {
	return model.TeamFixture{
		ID:              asStr(m, "id"),
		LeagueBracketID: asStr(m, "league_bracket_id"),
		TeamAID:         asStr(m, "team_a_id"),
		TeamBID:         asStr(m, "team_b_id"),
		WinnerTeamID:    asStr(m, "winner_team_id"),
		Score:           asStr(m, "score"),
		PlayedAt:        asStr(m, "played_at"),
	}
}

func mapFlexMatchup(m map[string]any) model.FlexMatchup {
	return model.FlexMatchup{
		ID:              asStr(m, "id"),
		LeagueBracketID: asStr(m, "league_bracket_id"),
		TeamAID:         asStr(m, "team_a_id"),
		TeamBID:         asStr(m, "team_b_id"),
		WinnerTeamID:    asStr(m, "winner_team_id"),
		Score:           asStr(m, "score"),
		Status:          asStr(m, "status"),
		PlayedAt:        asStr(m, "played_at"),
	}
}

func mapFinanceEntry(m map[string]any) model.FinanceEntry {
	return model.FinanceEntry{
		ID:          asStr(m, "id"),
		EventID:     asStr(m, "event_id"),
		Kind:        asStr(m, "kind"),
		Category:    asStr(m, "category"),
		AmountCents: asInt(m, "amount_cents"),
		Note:        asStr(m, "note"),
		CreatedAt:   asStr(m, "created_at"),
	}
}

func mapChecklistItem(m map[string]any) model.ChecklistItem {
	return model.ChecklistItem{
		ID:        asStr(m, "id"),
		EventID:   asStr(m, "event_id"),
		Label:     asStr(m, "label"),
		Checked:   asBool(m, "checked"),
		SortOrder: asInt(m, "sort_order"),
	}
}

func mapStanding(m map[string]any) model.Standing {
	return model.Standing{
		PlayerID:      asStr(m, "player_id"),
		FullName:      asStr(m, "full_name"),
		GamesPlayed:   asInt(m, "games_played"),
		Wins:          asInt(m, "wins"),
		Losses:        asInt(m, "losses"),
		PointsFor:     asInt(m, "points_for"),
		PointsAgainst: asInt(m, "points_against"),
		PointDiff:     asInt(m, "point_diff"),
	}
}

func mapRoundView(m map[string]any) model.RoundView {
	return model.RoundView{
		ID:          asStr(m, "id"),
		BracketID:   asStrPtr(m, "bracket_id"),
		RoundNumber: asInt(m, "round_number"),
		Status:      asStr(m, "status"),
	}
}

// matchSelect is the PostgREST select fragment for a match plus its resolved
// court number, round context, and participants (with player names) embedded —
// so a match and its sides load in one round-trip instead of N+1 queries.
const matchSelect = "id,bracket_id,stage,bracket_tier,bracket_group,bracket_round,bracket_slot," +
	"team1_score,team2_score,winning_team,games,status,result_type,play_order,duration_minutes,scheduled_day,completed_at,line_type," +
	"court:courts!court_id(court_number)," +
	"round:rounds!round_id(id,round_number,status,started_at)," +
	"participants:match_participants(team,player_id,player:players!player_id(full_name))"

func mapFeedItem(m map[string]any) model.FeedItem {
	fi := model.FeedItem{
		ID:        asStr(m, "id"),
		EventID:   asStr(m, "event_id"),
		Type:      asStr(m, "type"),
		Text:      asStr(m, "text"),
		ActorName: asStrPtr(m, "actor_name"),
		RefID:     asStrPtr(m, "ref_id"),
		CreatedAt: asStr(m, "created_at"),
	}
	// `event`-type posts stash the poster URL + start time in meta so the card
	// renders like the old upcoming-event card, but as a real, reactable item.
	if meta := asMap(m, "meta"); meta != nil {
		fi.PosterURL = asStrPtr(meta, "poster_url")
		fi.StartsAt = asStrPtr(meta, "starts_at")
	}
	return fi
}

func mapFeedComment(m map[string]any) model.FeedComment {
	return model.FeedComment{
		ID:         asStr(m, "id"),
		FeedItemID: asStr(m, "feed_item_id"),
		AuthorName: asStr(m, "author_name"),
		Text:       asStr(m, "text"),
		CreatedAt:  asStr(m, "created_at"),
	}
}

// mapSides turns the embedded match_participants array into ordered team sides.
func mapSides(m map[string]any) []model.Side {
	parts, _ := m["participants"].([]any)
	names := map[int][]string{}
	ids := map[int][]string{}
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		team := asInt(pm, "team")
		name := ""
		if pl := asMap(pm, "player"); pl != nil {
			name = asStr(pl, "full_name")
		}
		names[team] = append(names[team], name)
		ids[team] = append(ids[team], asStr(pm, "player_id"))
	}
	var sides []model.Side
	for _, t := range []int{1, 2} {
		if n, ok := names[t]; ok {
			sides = append(sides, model.Side{Team: t, Players: n, PlayerIDs: ids[t]})
		}
	}
	return sides
}

// mapMatch maps a match row (queried with matchSelect) to model.Match. Court and
// round context populate only when those embeds are present in the row.
// asGames decodes the matches.games jsonb (an array of {team1,team2}) into the
// per-game model. Returns nil for legacy single-game matches with no games column.
func asGames(m map[string]any, key string) []model.GameScore {
	raw, ok := m[key].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]model.GameScore, 0, len(raw))
	for _, r := range raw {
		if g, ok := r.(map[string]any); ok {
			out = append(out, model.GameScore{Team1: asInt(g, "team1"), Team2: asInt(g, "team2")})
		}
	}
	return out
}

func mapMatch(m map[string]any) model.Match {
	mt := model.Match{
		ID:              asStr(m, "id"),
		BracketID:       asStrPtr(m, "bracket_id"),
		Stage:           asStr(m, "stage"),
		BracketTier:     asStr(m, "bracket_tier"),
		BracketGroup:    asStr(m, "bracket_group"),
		BracketRound:    asIntPtr(m, "bracket_round"),
		BracketSlot:     asIntPtr(m, "bracket_slot"),
		Team1Score:      asIntPtr(m, "team1_score"),
		Team2Score:      asIntPtr(m, "team2_score"),
		WinningTeam:     asIntPtr(m, "winning_team"),
		Games:           asGames(m, "games"),
		Status:          asStr(m, "status"),
		ResultType:      asStr(m, "result_type"),
		PlayOrder:       asFloatPtr(m, "play_order"),
		DurationMinutes: asIntPtr(m, "duration_minutes"),
		ScheduledDay:    asIntPtr(m, "scheduled_day"),
		CompletedAt:     asStrPtr(m, "completed_at"),
		LineType:        asStr(m, "line_type"),
		Sides:           mapSides(m),
	}
	if c := asMap(m, "court"); c != nil {
		mt.CourtNumber = asIntPtr(c, "court_number")
	}
	if r := asMap(m, "round"); r != nil {
		mt.RoundID = asStrPtr(r, "id")
		mt.RoundNumber = asIntPtr(r, "round_number")
		mt.RoundStatus = asStr(r, "status")
		mt.RoundStartedAt = asStrPtr(r, "started_at")
	}
	return mt
}

// mapRegistration expects players(full_name,phone,dupr_id,dupr_rating) and
// brackets(min_rating,max_rating) embedded; it flags an out-of-band DUPR rating.
func mapRegistration(m map[string]any) model.Registration {
	r := model.Registration{
		ID:            asStr(m, "id"),
		EventID:       asStr(m, "event_id"),
		PlayerID:      asStr(m, "player_id"),
		BracketID:     asStrPtr(m, "bracket_id"),
		PaymentStatus: asStr(m, "payment_status"),
		CheckedIn:     asBool(m, "checked_in"),
		AddonTee:      asBool(m, "addon_tee"),
		AddonGrips:    asBool(m, "addon_grips"),
		CheckInToken:  asStrPtr(m, "check_in_token"),
		PartnerID:     asStrPtr(m, "partner_id"),
		// partner_name may be absent (column added by a later migration); asStrPtr
		// returns nil when the key is missing, so this is safe pre-migration.
		PartnerNote: asStrPtr(m, "partner_name"),
	}
	var skill *float64
	if p := asMap(m, "player"); p != nil {
		r.FullName = asStr(p, "full_name")
		r.Phone = asStr(p, "phone")
		r.DuprID = asStrPtr(p, "dupr_id")
		r.DuprRating = asFloatPtr(p, "dupr_rating")
		skill = asFloatPtr(p, "skill_level")
	}
	// Resolved name of a registered partner (via the partner_id FK embed).
	if pp := asMap(m, "partner"); pp != nil {
		r.PartnerName = asStrPtr(pp, "full_name")
	}
	// Effective rating: prefer DUPR, fall back to self-reported skill — the same
	// precedence RegisterPlayer uses, so this list view's OUTSIDE-DIVISION flag
	// matches the value computed at registration (incl. skill-only players).
	if b := asMap(m, "bracket"); b != nil {
		rating := r.DuprRating
		if rating == nil {
			rating = skill
		}
		if rating != nil {
			mn := asFloatPtr(b, "min_rating")
			mx := asFloatPtr(b, "max_rating")
			if (mn != nil && *rating < *mn) || (mx != nil && *rating > *mx) {
				r.OutsideRating = true
			}
		}
	}
	return r
}
