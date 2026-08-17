package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
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
// also the standings — the ladder order IS the ranking. Each entrant carries its
// win/loss/tie record, tallied from the division's recorded matches.
func (s *Service) ListLadder(leagueBracketID string) ([]model.LadderEntrant, error) {
	rows, err := s.sb.Select("ladder_entrants",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+"&select=*&order=position.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.LadderEntrant, 0, len(rows))
	idx := make(map[string]int, len(rows)) // entrant id → index in out
	for _, r := range rows {
		e := mapLadderEntrant(r)
		idx[e.ID] = len(out)
		out = append(out, e)
	}
	// Tally W/L/T from the match history (best-effort — a stats read failure must
	// not hide the ladder itself).
	matches, mErr := s.sb.Select("ladder_matches",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+
			"&select=entrant_a_id,entrant_b_id,winner_entrant_id")
	if mErr == nil {
		for _, m := range matches {
			a, b, w := asStr(m, "entrant_a_id"), asStr(m, "entrant_b_id"), asStr(m, "winner_entrant_id")
			if w == "" { // tie (null winner)
				if i, ok := idx[a]; ok {
					out[i].Ties++
				}
				if i, ok := idx[b]; ok {
					out[i].Ties++
				}
				continue
			}
			loser := a
			if w == a {
				loser = b
			}
			if i, ok := idx[w]; ok {
				out[i].Wins++
			}
			if i, ok := idx[loser]; ok {
				out[i].Losses++
			}
		}
	}
	return out, nil
}

// ladderReorderModel returns a ladder's configured reorder model ('leapfrog' or
// 'swap'), defaulting to 'leapfrog' when unset or pre-migration.
func (s *Service) ladderReorderModel(leagueBracketID string) string {
	if !s.columnReady("leagues", "ladder_reorder_model") {
		return "leapfrog"
	}
	row, err := s.sb.SelectOne("league_brackets",
		"id=eq."+store.Q(leagueBracketID)+"&select=league_id")
	if err != nil || row == nil {
		return "leapfrog"
	}
	lg, err := s.sb.SelectOne("leagues",
		"id=eq."+store.Q(asStr(row, "league_id"))+"&select=ladder_reorder_model")
	if err != nil || lg == nil {
		return "leapfrog"
	}
	if m := strings.TrimSpace(asStr(lg, "ladder_reorder_model")); m == "swap" {
		return "swap"
	}
	return "leapfrog"
}

// MoveLadderEntrant repositions an entrant to a new 1-based rank, shifting the
// entrants it passes so the ladder stays a clean 1..N permutation (atomic RPC).
// Lets an organizer reseed by rating or fix an order. Position is clamped to
// [1, N] server-side.
func (s *Service) MoveLadderEntrant(entrantID string, newPosition int) error {
	if strings.TrimSpace(entrantID) == "" {
		return errors.New("entrantId is required")
	}
	if newPosition < 1 {
		return errors.New("newPosition must be >= 1")
	}
	if _, err := s.sb.RPC("move_ladder_entrant", map[string]any{
		"p_entrant":      entrantID,
		"p_new_position": newPosition,
	}); err != nil {
		return err
	}
	// The moved entrant's relative positions changed → any open challenge it's in
	// is now stale; void + notify rather than resolve against a moved ladder.
	s.voidActiveChallengesForEntrant(entrantID,
		"The ladder was reordered — your open challenge was cancelled.")
	return nil
}

// AddLadderEntrant appends an entrant to the BOTTOM of a division's ladder
// (position = current count + 1). Returns the new entrant. The division must
// exist (the caller already proved league ownership).
func (s *Service) AddLadderEntrant(leagueBracketID string, req model.AddLadderEntrantRequest) (model.LadderEntrant, error) {
	if strings.TrimSpace(req.DisplayName) == "" {
		return model.LadderEntrant{}, errors.New("displayName is required")
	}
	// A linked player can only be on a ladder ONCE. Callers used to each carry
	// their own check (join did, the seeder did, the sync did) — and any two of
	// them racing still inserted twice, because the check lived outside this
	// function. Doing it here covers every caller; a re-add returns the existing
	// entrant, which is what "add someone who's already on" should mean.
	if pid := strings.TrimSpace(strOr(req.PlayerID)); pid != "" {
		if ex, _ := s.sb.SelectOne("ladder_entrants",
			"league_bracket_id=eq."+store.Q(leagueBracketID)+
				"&player_id=eq."+store.Q(pid)+"&select=*&limit=1"); ex != nil {
			return mapLadderEntrant(ex), nil
		}
	}
	// A name already on this ladder is almost always the same human being added
	// twice — the ladder is typed at the door, and "Jen W" tonight is "Jen
	// Whitfield" next week. Refuse ONCE so it's seen; the organizer confirms if
	// they really do have two Chrises.
	if !req.AllowDuplicateName &&
		s.ladderNameTaken(leagueBracketID, req.DisplayName) {
		return model.LadderEntrant{}, fmt.Errorf("%w: %s is already on this ladder",
			ErrDuplicateName, strings.TrimSpace(req.DisplayName))
	}
	// A new entrant joins at the bottom: position = (max existing) + 1. Reading
	// the current count is fine for an organizer-driven, single-writer flow; the
	// DEFERRABLE unique(position) constraint backstops any race.
	existing, err := s.ListLadder(leagueBracketID)
	if err != nil {
		return model.LadderEntrant{}, err
	}
	// Bottom slot = (highest existing position) + 1, NOT count+1: if a prior
	// removal's compaction partially failed it can leave a gap, and count+1 would
	// then collide with a surviving higher position. ListLadder is ordered
	// position.asc, so the last entry holds the max.
	pos := 1
	if len(existing) > 0 {
		pos = existing[len(existing)-1].Position + 1
	}
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
// Delete + compaction run ATOMICALLY in the remove_ladder_entrant() RPC (one
// transaction), so a partial failure can never leave a permanent position gap.
func (s *Service) RemoveLadderEntrant(entrantID string) error {
	if strings.TrimSpace(entrantID) == "" {
		return errors.New("entrantId is required")
	}
	// Notify + void the entrant's active challenges BEFORE the delete cascades
	// them away silently (so the surviving counterparty learns why).
	s.voidActiveChallengesForEntrant(entrantID,
		"The other player left the ladder — your challenge was cancelled.")
	// Read the player id BEFORE the delete — afterwards there's nothing to read,
	// and we need it to undo the registration the roster sync created.
	var playerID, divisionID string
	if row, rerr := s.sb.SelectOne("ladder_entrants",
		"id=eq."+store.Q(entrantID)+"&select=player_id,league_bracket_id"); rerr == nil &&
		row != nil {
		playerID = asStr(row, "player_id")
		divisionID = asStr(row, "league_bracket_id")
	}
	if _, err := s.sb.RPC("remove_ladder_entrant",
		map[string]any{"p_entrant": entrantID}); err != nil {
		return err
	}
	// Undo the mirror. syncLadderRegistrations creates a registration for every
	// entrant, so leaving it behind would keep someone on the Players tab after
	// they'd been removed from the ladder — the button would look like it did
	// nothing, and the two rosters would disagree again.
	s.removeLadderMirrorRegistration(divisionID, playerID)
	return nil
}

// removeLadderMirrorRegistration deletes the registration that the roster sync
// created for a ladder entrant, on the league's ongoing event.
//
// Best-effort and deliberately narrow: it will NOT delete a registration that
// has been paid. Money is a record, and a roster edit should not erase one —
// an organizer removing a player who paid dues needs to see that payment, not
// discover it vanished. Those stay, and can be dealt with in Finances.
func (s *Service) removeLadderMirrorRegistration(divisionID, playerID string) {
	if divisionID == "" || playerID == "" {
		return
	}
	leagueID, err := s.LeagueIDOfDivision(divisionID)
	if err != nil || leagueID == "" {
		return
	}
	// ONLY the perpetual event — the one syncLadderRegistrations writes to.
	//
	// This used to take the league's first five events and delete the player's
	// registration on each. For a ladder that's one perpetual event, so it
	// looked right; for a league carrying BOTH a ladder and dated session
	// events, removing someone from the ladder would have stripped their
	// registrations from sessions that have nothing to do with it. Undo exactly
	// what the mirror created, and nothing else.
	rows, err := s.sb.Select("events",
		"league_id=eq."+store.Q(leagueID)+"&perpetual=is.true&select=id&limit=1")
	if err != nil {
		return
	}
	for _, ev := range rows {
		eid := asStr(ev, "id")
		if eid == "" {
			continue
		}
		reg, rerr := s.sb.SelectOne("registrations",
			"event_id=eq."+store.Q(eid)+"&player_id=eq."+store.Q(playerID)+
				"&select=id,payment_status")
		if rerr != nil || reg == nil {
			continue
		}
		if st := strings.ToLower(asStr(reg, "payment_status")); st == "paid" {
			log.Printf("ladder: kept PAID registration %s after removing entrant",
				asStr(reg, "id"))
			continue
		}
		if derr := s.sb.Delete("registrations",
			"id=eq."+store.Q(asStr(reg, "id"))); derr != nil {
			log.Printf("ladder: could not remove mirrored registration: %v", derr)
		}
	}
}

// RecordLadderResult records a match between two entrants and applies the
// leapfrog reorder ATOMICALLY via the apply_ladder_result() plpgsql function
// (one transaction). WinnerEntrantID must be one of A/B. Returns the new match.
func (s *Service) RecordLadderResult(leagueBracketID string, req model.RecordLadderResultRequest) (model.LadderMatch, error) {
	a := strings.TrimSpace(req.EntrantAID)
	b := strings.TrimSpace(req.EntrantBID)
	w := strings.TrimSpace(req.WinnerEntrantID)
	if a == "" || b == "" {
		return model.LadderMatch{}, errors.New("entrantAId and entrantBId are required")
	}
	if a == b {
		return model.LadderMatch{}, errors.New("the two entrants must be different")
	}
	// A tie is an explicit flag OR simply no winner supplied. Otherwise the winner
	// must be one of the two sides.
	tie := req.Tie || w == ""
	if !tie && w != a && w != b {
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

	payload := map[string]any{
		"p_entrant_a":     a,
		"p_entrant_b":     b,
		"p_reorder_model": s.ladderReorderModel(leagueBracketID),
		"p_score":         orNull(strings.TrimSpace(req.Score)),
	}
	// Omit p_winner on a tie → the RPC default (null) records a tie with no reorder.
	if !tie {
		payload["p_winner"] = w
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
	// An organizer-recorded result moved these entrants → void their open
	// challenges (they'd otherwise resolve against stale positions).
	s.voidActiveChallengesForEntrant(a, "The organizer recorded a result — your open challenge was cancelled.")
	s.voidActiveChallengesForEntrant(b, "The organizer recorded a result — your open challenge was cancelled.")
	return mapLadderMatch(mrow), nil
}

// ladderConfigColumns normalizes a LadderConfig into its leagues-table columns,
// clamping to safe values so bad input can't poison the ladder rules.
func ladderConfigColumns(c model.LadderConfig) map[string]any {
	reorder := "leapfrog"
	if c.ReorderModel == "swap" {
		reorder = "swap"
	}
	action := "none"
	switch c.InactivityAction {
	case "drop_one", "drop_bottom":
		action = c.InactivityAction
	}
	nonNeg := func(n int) int {
		if n < 0 {
			return 0
		}
		return n
	}
	// A 0-day respond/play window is nonsensical (it would expire instantly) and
	// usually just means the field was omitted by a non-Flutter client, so fall
	// back to the researched 7/14 defaults rather than writing 0. challengeRange
	// and inactivityDays legitimately use 0 (unlimited / off), so they keep it.
	daysOr := func(n, def int) int {
		if n <= 0 {
			return def
		}
		return n
	}
	return map[string]any{
		"ladder_reorder_model":     reorder,
		"ladder_challenge_range":   nonNeg(c.ChallengeRange),
		"ladder_response_days":     daysOr(c.ResponseDays, 7),
		"ladder_play_days":         daysOr(c.PlayDays, 14),
		"ladder_inactivity_days":   nonNeg(c.InactivityDays),
		"ladder_inactivity_action": action,
	}
}

// SetLadderConfig updates a ladder league's rule config (owner-gated at the HTTP
// layer). No-op-safe pre-migration (columnReady guard).
func (s *Service) SetLadderConfig(leagueID string, cfg model.LadderConfig) error {
	if strings.TrimSpace(leagueID) == "" {
		return errors.New("leagueId is required")
	}
	if !s.columnReady("leagues", "ladder_reorder_model") {
		return errors.New("ladder config is not available yet")
	}
	_, err := s.sb.Update("leagues", "id=eq."+store.Q(leagueID), ladderConfigColumns(cfg))
	return err
}

// SetLadderFormat switches a ladder league between the 'challenge' and
// 'rotation' formats (owner-gated at the HTTP layer). Only meaningful for ladder
// leagues; no-op-safe pre-migration (columnReady guard).
func (s *Service) SetLadderFormat(leagueID, format string) error {
	if strings.TrimSpace(leagueID) == "" {
		return errors.New("leagueId is required")
	}
	if format != "rotation" {
		format = "challenge"
	}
	if !s.columnReady("leagues", "ladder_format") {
		return errors.New("ladder format switching is not available yet")
	}
	_, err := s.sb.Update("leagues", "id=eq."+store.Q(leagueID),
		map[string]any{"ladder_format": format})
	return err
}

// LadderHistory returns a division's recorded matches, newest first.
func (s *Service) LadderHistory(leagueBracketID string) ([]model.LadderMatch, error) {
	rows, err := s.sb.Select("ladder_matches",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+
			"&select=*&order=played_at.desc,created_at.desc")
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

// swapReorder is the PURE in-memory model of the SWAP rule (the alternate to
// leapfrog, selectable via the ladder's reorder model). When the LOWER-ranked
// entrant wins, the two entrants simply exchange positions and NOBODY else
// moves; when the higher-ranked entrant wins (or ids are absent / equal), the
// order is unchanged. Mirrors the 'swap' branch of apply_ladder_result().
func swapReorder(order []string, winnerID, loserID string) []string {
	out := make([]string, len(order))
	copy(out, order)
	if winnerID == loserID {
		return out
	}
	wi, li := indexOf(out, winnerID), indexOf(out, loserID)
	if wi < 0 || li < 0 || wi <= li {
		return out // absent, or higher-ranked already won → no change
	}
	out[wi], out[li] = out[li], out[wi]
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

// --- registered-but-not-on-the-ladder -----------------------------------------

// LadderSyncStatus is how many of a league's registered players are missing
// from this division's ladder.
type LadderSyncStatus struct {
	// Missing is registered players with no entrant on this ladder.
	Missing int `json:"missing"`
	// OnLadder is the current entrant count, so the caller can phrase the whole
	// sentence ("26 on the ladder · 4 registered players aren't").
	OnLadder int `json:"onLadder"`
}

// missingLadderPlayers returns the league's registered players who have no
// entrant on this division's ladder, as playerID -> display name.
//
// Registering somebody and putting them on the ladder are separate acts:
// RegisterPlayer never touches ladder_entrants, and the only routes onto a
// ladder are the organizer typing a name or the player self-joining the ladder
// link. So an organizer can register twenty people, open the Rotation tab to
// start a night, and find nobody to pick from.
//
// This is deliberately a QUERY, not an automatic sync. Auto-adding on register
// would force an answer to "does unregistering remove them?" — either wiping
// somebody's ladder standing from an action that doesn't look destructive, or
// accumulating ghosts — and would silently map event brackets onto ladder
// divisions, and silently put +1555 placeholders on the ladder. An organizer
// pressing a button that says how many is legible; all of that is not.
// missingEntry is one registered player absent from the league's ladders.
type missingEntry struct {
	playerID string
	name     string
}

func (s *Service) missingLadderPlayers(div string) ([]missingEntry, int, error) {
	row, err := s.sb.SelectOne("league_brackets",
		"id=eq."+store.Q(div)+"&select=league_id")
	if err != nil {
		return nil, 0, err
	}
	if row == nil {
		return nil, 0, ErrNotFound
	}
	leagueID := asStr(row, "league_id")
	if leagueID == "" {
		return nil, 0, nil
	}
	evs, err := s.sb.Select("events", "league_id=eq."+store.Q(leagueID)+"&select=id")
	if err != nil {
		return nil, 0, err
	}
	eventIDs := make([]string, 0, len(evs))
	for _, e := range evs {
		if id := asStr(e, "id"); id != "" {
			eventIDs = append(eventIDs, id)
		}
	}
	// Dedupe against EVERY division's ladder, not just the pressed one.
	//
	// Registrations belong to the league; ladders belong to divisions; nothing
	// maps one to the other. Checking only the pressed division meant a
	// two-division league reported (and on Add, inserted) division B's players
	// as "missing" from division A — pressing the button on each tab in turn
	// would put the entire league on both ladders. Cross-division dedupe keeps
	// a human on at most one ladder; WHICH ladder a genuinely missing player
	// joins is the division whose tab the organizer pressed, which is the one
	// piece of mapping only they can decide.
	divs, err := s.sb.Select("league_brackets",
		"league_id=eq."+store.Q(leagueID)+"&select=id")
	if err != nil {
		return nil, 0, err
	}
	onLadder := map[string]bool{}
	onThisLadder := 0
	for _, d := range divs {
		divID := asStr(d, "id")
		if divID == "" {
			continue
		}
		entrants, lerr := s.ListLadder(divID)
		if lerr != nil {
			return nil, 0, lerr
		}
		if divID == div {
			onThisLadder = len(entrants)
		}
		for _, e := range entrants {
			if e.PlayerID != nil && *e.PlayerID != "" {
				onLadder[*e.PlayerID] = true
			}
		}
	}
	if len(eventIDs) == 0 {
		return nil, onThisLadder, nil
	}
	// PAGE through the registrations. PostgREST silently caps a single response
	// at ~1000 rows regardless of the limit written on the URL, so one read of
	// "limit=2000" quietly dropped everyone past the cap — and a dropped row
	// here means a player who never shows up as missing at all.
	//
	// Approval matters too: the sync must not promote a PENDING registration
	// onto the ladder. The approval queue exists so an organizer decides who's
	// in; a side door through the ladder would make that queue decorative.
	seen := map[string]bool{}
	missing := []missingEntry{}
	const page = 1000
	for offset := 0; offset < 10000; offset += page {
		regs, rerr := s.sb.Select("registrations",
			"event_id="+store.In(eventIDs)+s.approvedRegFilter()+
				"&select=player_id,player:players!player_id(full_name,phone)"+
				fmt.Sprintf("&limit=%d&offset=%d", page, offset))
		if rerr != nil {
			return nil, 0, rerr
		}
		for _, r := range regs {
			pid := asStr(r, "player_id")
			if pid == "" || onLadder[pid] || seen[pid] {
				continue
			}
			seen[pid] = true
			pl := asMap(r, "player")
			if pl == nil {
				continue
			}
			// Placeholders (the +1555 test rows) never belong on a real
			// ladder — they'd render on the public TV board and in standings.
			if strings.HasPrefix(asStr(pl, "phone"), "+15553") {
				continue
			}
			name := strings.TrimSpace(asStr(pl, "full_name"))
			if name == "" {
				// A ladder entrant must have a name — the board, the standings
				// and the spoken court calls all render it.
				continue
			}
			missing = append(missing, missingEntry{playerID: pid, name: name})
		}
		if len(regs) < page {
			break
		}
	}
	// Deterministic order. Iterating a map meant every press appended the
	// missing players to the bottom of the ladder in a DIFFERENT random order —
	// and bottom-of-ladder position is a real thing on a ladder. Alphabetical
	// is at least explainable to the people standing at the bottom.
	sort.Slice(missing, func(i, j int) bool { return missing[i].name < missing[j].name })
	return missing, onThisLadder, nil
}

// LadderSync reports how many registered players aren't on this ladder.
func (s *Service) LadderSync(div string) (LadderSyncStatus, error) {
	missing, onLadder, err := s.missingLadderPlayers(div)
	if err != nil {
		return LadderSyncStatus{}, err
	}
	return LadderSyncStatus{Missing: len(missing), OnLadder: onLadder}, nil
}

// AddRegisteredToLadder puts every registered player who isn't on any of the
// league's ladders onto THIS division's, and returns how many were added.
//
// Idempotent by construction: it only ever adds players missing at the moment
// it runs, and AddLadderEntrant itself refuses to duplicate a linked player,
// so pressing it twice — even concurrently — adds nothing the second time.
func (s *Service) AddRegisteredToLadder(div string) (int, error) {
	missing, _, err := s.missingLadderPlayers(div)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, m := range missing {
		p := m.playerID
		if _, err := s.AddLadderEntrant(div, model.AddLadderEntrantRequest{
			DisplayName: m.name,
			PlayerID:    &p,
		}); err != nil {
			// Keep going: one bad row shouldn't strand the other nineteen. The
			// count returned is what actually landed.
			log.Printf("ladder sync: could not add %s (%s): %v", m.name, p, err)
			continue
		}
		added++
	}
	return added, nil
}
