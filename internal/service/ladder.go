package service

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// ----------------------------------------------------------------------------
// Ladder League (engine #2 of 4).
//
// A ladder is an ORDERED ranking of entrants per league division
// (league_brackets row): position 1 = top. The organizer adds entrants (a new
// entrant joins at the bottom) and records a match result between two entrants.
//
// Leapfrog rule: if a LOWER-ranked entrant beats a HIGHER-ranked one, the
// winner moves into the loser's position and everyone between shifts down one;
// if the higher-ranked entrant wins, positions are unchanged. The match is
// recorded either way. Organizer-driven only — player self-service challenges
// are a FUTURE v2.
//
// Standings == the current ladder order (ListLadder). The actual reorder is
// applied ATOMICALLY by the apply_ladder_result() plpgsql function (0030); the
// pure leapfrogReorder() below mirrors that logic for unit testing.
// ----------------------------------------------------------------------------

// LadderOwner returns the owner (auth-user id) of the league behind a division
// (league_bracket), so the HTTP layer can gate ladder writes on league
// ownership. Returns ErrNotFound if the division or its league is missing.
func (s *Service) LadderOwner(leagueBracketID string) (string, error) {
	row, err := s.sb.SelectOne("league_brackets",
		"id=eq."+store.Q(leagueBracketID)+"&select=league_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return s.OwnerOf("league", asStr(row, "league_id"))
}

// LadderOwnerOfEntrant resolves the owning league's owner from an entrant id (an
// entrant → its division → its league → owner). Used to gate per-entrant writes.
func (s *Service) LadderOwnerOfEntrant(entrantID string) (string, error) {
	div, err := s.divisionOfEntrant(entrantID)
	if err != nil {
		return "", err
	}
	return s.LadderOwner(div)
}

// divisionOfEntrant returns the league_bracket_id an entrant belongs to.
func (s *Service) divisionOfEntrant(entrantID string) (string, error) {
	row, err := s.sb.SelectOne("ladder_entrants",
		"id=eq."+store.Q(entrantID)+"&select=league_bracket_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return asStr(row, "league_bracket_id"), nil
}

// ListLadder returns a division's ladder, ordered by position (1 = top). This is
// also the standings — the ladder order IS the ranking.
func (s *Service) ListLadder(leagueBracketID string) ([]model.LadderEntrant, error) {
	rows, err := s.sb.Select("ladder_entrants",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+"&select=*&order=position.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.LadderEntrant, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapLadderEntrant(r))
	}
	return out, nil
}

// AddLadderEntrant appends an entrant to the BOTTOM of a division's ladder
// (position = current count + 1). Returns the new entrant. The division must
// exist (the caller already proved league ownership).
func (s *Service) AddLadderEntrant(leagueBracketID string, req model.AddLadderEntrantRequest) (model.LadderEntrant, error) {
	if strings.TrimSpace(req.DisplayName) == "" {
		return model.LadderEntrant{}, errors.New("displayName is required")
	}
	// A new entrant joins at the bottom: position = (max existing) + 1. Reading
	// the current count is fine for an organizer-driven, single-writer flow; the
	// DEFERRABLE unique(position) constraint backstops any race.
	existing, err := s.ListLadder(leagueBracketID)
	if err != nil {
		return model.LadderEntrant{}, err
	}
	pos := len(existing) + 1
	rows, err := s.sb.Insert("ladder_entrants", map[string]any{
		"league_bracket_id": leagueBracketID,
		"display_name":      strings.TrimSpace(req.DisplayName),
		"player_id":         orNull(strOr(req.PlayerID)),
		"is_team":           req.IsTeam,
		"position":          pos,
	})
	if err != nil {
		return model.LadderEntrant{}, err
	}
	if len(rows) == 0 {
		return model.LadderEntrant{}, errors.New("ladder entrant insert returned no row")
	}
	return mapLadderEntrant(rows[0]), nil
}

// RemoveLadderEntrant deletes an entrant and CLOSES the gap so the ladder stays
// a contiguous 1..N permutation: every entrant below the removed one moves up
// by one. The entrant's match history cascade-deletes (FK on delete cascade).
func (s *Service) RemoveLadderEntrant(entrantID string) error {
	div, err := s.divisionOfEntrant(entrantID)
	if err != nil {
		return err
	}
	// Read the removed entrant's position so we can compact below it.
	row, err := s.sb.SelectOne("ladder_entrants",
		"id=eq."+store.Q(entrantID)+"&select=position")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	removedPos := asInt(row, "position")

	if err := s.sb.Delete("ladder_entrants", "id=eq."+store.Q(entrantID)); err != nil {
		return err
	}

	// Compact: everyone below the removed slot shifts up one. Done as a single
	// ordered pass (lowest first) so the unique(position) constraint never trips.
	below, err := s.sb.Select("ladder_entrants",
		"league_bracket_id=eq."+store.Q(div)+
			"&position=gt."+store.Q(strconv.Itoa(removedPos))+
			"&select=id,position&order=position.asc")
	if err != nil {
		return err
	}
	for _, e := range below {
		id := asStr(e, "id")
		newPos := asInt(e, "position") - 1
		if _, uerr := s.sb.Update("ladder_entrants", "id=eq."+store.Q(id),
			map[string]any{"position": newPos}); uerr != nil {
			return uerr
		}
	}
	return nil
}

// RecordLadderResult records a match between two entrants and applies the
// leapfrog reorder ATOMICALLY via the apply_ladder_result() plpgsql function
// (one transaction). WinnerEntrantID must be one of A/B. Returns the new match.
func (s *Service) RecordLadderResult(leagueBracketID string, req model.RecordLadderResultRequest) (model.LadderMatch, error) {
	a := strings.TrimSpace(req.EntrantAID)
	b := strings.TrimSpace(req.EntrantBID)
	w := strings.TrimSpace(req.WinnerEntrantID)
	if a == "" || b == "" || w == "" {
		return model.LadderMatch{}, errors.New("entrantAId, entrantBId and winnerEntrantId are required")
	}
	if a == b {
		return model.LadderMatch{}, errors.New("the two entrants must be different")
	}
	if w != a && w != b {
		return model.LadderMatch{}, errors.New("winnerEntrantId must be entrantAId or entrantBId")
	}
	// Bind the mutation to the authorized (path) division: BOTH entrants must
	// belong to it. Authorization is gated on the path division id, but the RPC
	// derives the division from the caller-supplied entrant ids — without this
	// check an owner of any ladder could reorder a different organizer's ladder
	// by passing their entrant ids (IDOR / confused deputy).
	for _, id := range []string{a, b} {
		div, derr := s.divisionOfEntrant(id)
		if derr != nil {
			return model.LadderMatch{}, derr
		}
		if div != leagueBracketID {
			return model.LadderMatch{}, errors.New("entrant does not belong to this division")
		}
	}
	loser := a
	if w == a {
		loser = b
	}

	payload := map[string]any{
		"p_winner_entrant_id": w,
		"p_loser_entrant_id":  loser,
		"p_score":             orNull(strings.TrimSpace(req.Score)),
	}
	if pa := strings.TrimSpace(req.PlayedAt); pa != "" {
		payload["p_played_at"] = pa
	}
	body, err := s.sb.RPC("apply_ladder_result", payload)
	if err != nil {
		return model.LadderMatch{}, err
	}
	// apply_ladder_result returns the new match id (a bare JSON uuid string).
	var matchID string
	if err := json.Unmarshal(body, &matchID); err != nil || matchID == "" {
		return model.LadderMatch{}, errors.New("ladder result RPC returned no match id")
	}
	mrow, err := s.sb.SelectOne("ladder_matches", "id=eq."+store.Q(matchID)+"&select=*")
	if err != nil {
		return model.LadderMatch{}, err
	}
	if mrow == nil {
		return model.LadderMatch{}, ErrNotFound
	}
	return mapLadderMatch(mrow), nil
}

// LadderHistory returns a division's recorded matches, newest first.
func (s *Service) LadderHistory(leagueBracketID string) ([]model.LadderMatch, error) {
	rows, err := s.sb.Select("ladder_matches",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+"&select=*&order=played_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.LadderMatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapLadderMatch(r))
	}
	return out, nil
}

// leapfrogReorder is the PURE in-memory model of the ladder leapfrog rule (the
// same logic apply_ladder_result() runs in SQL). Given a ladder ordered top→
// bottom and the winner/loser of a match, it returns the new top→bottom order.
//
//   - If the winner is the LOWER-ranked entrant (appears AFTER the loser), the
//     winner moves into the loser's slot and everyone from the loser down to
//     (but not including) the winner shifts down one.
//   - If the winner is already AT OR ABOVE the loser (higher-ranked won), the
//     order is unchanged.
//
// ids that aren't present, or winner==loser, return the input order unchanged.
func leapfrogReorder(order []string, winnerID, loserID string) []string {
	out := make([]string, len(order))
	copy(out, order)
	if winnerID == loserID {
		return out
	}
	wi, li := indexOf(out, winnerID), indexOf(out, loserID)
	if wi < 0 || li < 0 {
		return out
	}
	// Higher-ranked (smaller index) won, or they're the same slot: no change.
	if wi <= li {
		return out
	}
	// Lower-ranked winner leapfrogs: pull the winner out, then insert it at the
	// loser's index, shifting [li, wi) down one.
	winner := out[wi]
	// Shift the block [li, wi) one step toward the bottom.
	copy(out[li+1:wi+1], out[li:wi])
	out[li] = winner
	return out
}

func indexOf(ss []string, v string) int {
	for i, s := range ss {
		if s == v {
			return i
		}
	}
	return -1
}
