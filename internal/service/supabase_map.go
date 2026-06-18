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
		ID:                   asStr(m, "id"),
		Name:                 asStr(m, "name"),
		Format:               asStr(m, "format"),
		PartnerMode:          asStr(m, "partner_mode"),
		TournamentFormat:     asStr(m, "tournament_format"),
		ScoringMode:          asStr(m, "scoring_mode"),
		NumCourts:            asInt(m, "num_courts"),
		PointsToWin:          asInt(m, "points_to_win"),
		WinBy:                asInt(m, "win_by"),
		GameDurationMinutes:  asInt(m, "game_duration_minutes"),
		RegistrationFeeCents: asInt(m, "registration_fee_cents"),
		Currency:             asStr(m, "currency"),
		Location:             asStrPtr(m, "location"),
		VenueName:            asStrPtr(m, "venue_name"),
		VenueAddress:         asStrPtr(m, "venue_address"),
		VenuePhone:           asStrPtr(m, "venue_phone"),
		VenueWebsite:         asStrPtr(m, "venue_website"),
		VenueLat:             asFloatPtr(m, "venue_lat"),
		VenueLng:             asFloatPtr(m, "venue_lng"),
		DuprSanctioned:       asBool(m, "dupr_sanctioned"),
		StartsAt:             asStrPtr(m, "starts_at"),
		EndsAt:               asStrPtr(m, "ends_at"),
		Description:          asStrPtr(m, "description"),
		SlotDurations:        asIntMap(m, "slot_durations"),
		Status:               asStr(m, "status"),
	}
}

// asIntMap reads a jsonb object column of { "<key>": number } into map[string]int.
func asIntMap(m map[string]any, k string) map[string]int {
	raw, ok := m[k].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]int, len(raw))
	for key, v := range raw {
		if n, ok := v.(float64); ok {
			out[key] = int(n)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapBracket(m map[string]any) model.Bracket {
	return model.Bracket{
		ID:        asStr(m, "id"),
		EventID:   asStr(m, "event_id"),
		Name:      asStr(m, "name"),
		MinRating: asFloatPtr(m, "min_rating"),
		MaxRating: asFloatPtr(m, "max_rating"),
		MinAge:    asIntPtr(m, "min_age"),
		MaxAge:    asIntPtr(m, "max_age"),
		SortOrder: asInt(m, "sort_order"),
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
const matchSelect = "id,bracket_id,stage,bracket_round,bracket_slot," +
	"team1_score,team2_score,winning_team,status,result_type,play_order,duration_minutes," +
	"court:courts!court_id(court_number)," +
	"round:rounds!round_id(id,round_number,status)," +
	"participants:match_participants(team,player_id,player:players!player_id(full_name))"

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
func mapMatch(m map[string]any) model.Match {
	mt := model.Match{
		ID:           asStr(m, "id"),
		BracketID:    asStrPtr(m, "bracket_id"),
		Stage:        asStr(m, "stage"),
		BracketRound: asIntPtr(m, "bracket_round"),
		BracketSlot:  asIntPtr(m, "bracket_slot"),
		Team1Score:   asIntPtr(m, "team1_score"),
		Team2Score:   asIntPtr(m, "team2_score"),
		WinningTeam:  asIntPtr(m, "winning_team"),
		Status:       asStr(m, "status"),
		ResultType:      asStr(m, "result_type"),
		PlayOrder:       asFloatPtr(m, "play_order"),
		DurationMinutes: asIntPtr(m, "duration_minutes"),
		Sides:           mapSides(m),
	}
	if c := asMap(m, "court"); c != nil {
		mt.CourtNumber = asIntPtr(c, "court_number")
	}
	if r := asMap(m, "round"); r != nil {
		mt.RoundID = asStrPtr(r, "id")
		mt.RoundNumber = asIntPtr(r, "round_number")
		mt.RoundStatus = asStr(r, "status")
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
		CheckInToken:  asStrPtr(m, "check_in_token"),
	}
	var skill *float64
	if p := asMap(m, "player"); p != nil {
		r.FullName = asStr(p, "full_name")
		r.Phone = asStr(p, "phone")
		r.DuprID = asStrPtr(p, "dupr_id")
		r.DuprRating = asFloatPtr(p, "dupr_rating")
		skill = asFloatPtr(p, "skill_level")
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
