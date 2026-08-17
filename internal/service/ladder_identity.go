package service

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// ErrDuplicateName is returned when an add would put a name on a ladder that
// already lists it. A refusal the caller can override (see
// AddLadderEntrantRequest.AllowDuplicateName), not a hard rule — two real
// people can share a name, and the organizer is the one who knows.
var ErrDuplicateName = errors.New("that name is already on this ladder")

// Who a ladder entrant IS — renaming them, and merging the ones that turned out
// to be the same person.
//
// A ladder is meant to be the thing that persists between sessions, but nothing
// could correct an identity once it existed: a typo'd name re-imported into
// every future session forever, and "Jen W" and "Jen Whitfield" sat on the
// ladder as two separate people with the record of one night each. Both are
// certainties on a roster typed at the door on a Tuesday.

// entrantRefs are every column that points at a ladder entrant.
//
// Kept as data rather than as five copies of the same UPDATE, because the merge
// is only correct if it repoints ALL of them — a missed column leaves a
// dangling reference to a row that is about to be deleted.
var entrantRefs = []struct{ table, column string }{
	{"ladder_matches", "entrant_a_id"},
	{"ladder_matches", "entrant_b_id"},
	{"ladder_matches", "winner_entrant_id"},
	{"ladder_challenges", "challenger_entrant_id"},
	{"ladder_challenges", "challenged_entrant_id"},
	{"rotation_players", "entrant_id"},
}

// SetLadderEntrantName renames an entrant on the ladder itself.
//
// There was no way to do this at all. A rotation session could rename its own
// roster row, but that copy dies with the session — so the misspelling on the
// ladder survived, and re-imported wrong into every session after it. On a
// challenge ladder, which has no session to rename through, a typo was simply
// permanent.
func (s *Service) SetLadderEntrantName(entrantID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a name is required")
	}
	rows, err := s.sb.Update("ladder_entrants",
		"id=eq."+store.Q(entrantID),
		map[string]any{"display_name": name})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrNotFound
	}
	return nil
}

// MergeLadderEntrants folds [mergeID] into [keepID]: every match, challenge and
// session appearance moves to the survivor, and the duplicate is removed.
//
// The survivor keeps its own name and position — a merge is "these were always
// the same person", so the record combines and the ladder does not shuffle.
func (s *Service) MergeLadderEntrants(keepID, mergeID string) error {
	keepID, mergeID = strings.TrimSpace(keepID), strings.TrimSpace(mergeID)
	if keepID == "" || mergeID == "" {
		return errors.New("both entrants are required")
	}
	if keepID == mergeID {
		return errors.New("that's the same entrant twice")
	}
	keep, err := s.sb.SelectOne("ladder_entrants",
		"id=eq."+store.Q(keepID)+"&select=id,league_bracket_id,player_id")
	if err != nil {
		return err
	}
	loser, err := s.sb.SelectOne("ladder_entrants",
		"id=eq."+store.Q(mergeID)+"&select=id,league_bracket_id,player_id")
	if err != nil {
		return err
	}
	if keep == nil || loser == nil {
		return ErrNotFound
	}
	// Same ladder only. Merging across divisions would move one division's
	// results into another's standings, which is not a correction of anything.
	if asStr(keep, "league_bracket_id") != asStr(loser, "league_bracket_id") {
		return errors.New("those two are on different ladders")
	}

	// Repoint everything BEFORE the delete. Doing it the other way round loses
	// the history to the cascade, which is the one thing a merge exists to keep.
	for _, ref := range entrantRefs {
		if _, uerr := s.sb.Update(ref.table,
			ref.column+"=eq."+store.Q(mergeID),
			map[string]any{ref.column: keepID}); uerr != nil {
			return fmt.Errorf("couldn't move %s.%s: %w", ref.table, ref.column, uerr)
		}
	}

	// Anything that now points at the survivor on BOTH sides was a game or a
	// challenge between the two duplicates — i.e. between one person and
	// themselves. That isn't a result to keep; it's an artefact of the split.
	if err := s.sb.Delete("ladder_matches",
		"entrant_a_id=eq."+store.Q(keepID)+
			"&entrant_b_id=eq."+store.Q(keepID)); err != nil {
		return fmt.Errorf("couldn't clear self-matches: %w", err)
	}
	if err := s.sb.Delete("ladder_challenges",
		"challenger_entrant_id=eq."+store.Q(keepID)+
			"&challenged_entrant_id=eq."+store.Q(keepID)); err != nil {
		return fmt.Errorf("couldn't clear self-challenges: %w", err)
	}

	// If only the duplicate carried the account link, the survivor inherits it —
	// otherwise merging would disconnect a real player from their own ladder row
	// and they'd lose the claim, the push targeting and their app view of it.
	if asStr(keep, "player_id") == "" && asStr(loser, "player_id") != "" {
		if _, uerr := s.sb.Update("ladder_entrants",
			"id=eq."+store.Q(keepID),
			map[string]any{"player_id": asStr(loser, "player_id")}); uerr != nil {
			return fmt.Errorf("couldn't move the account link: %w", uerr)
		}
		// Drop it from the loser first, or the unique (division, player) index
		// added alongside this refuses to let both rows hold it for the instant
		// between the two writes.
		if _, uerr := s.sb.Update("ladder_entrants",
			"id=eq."+store.Q(mergeID),
			map[string]any{"player_id": nil}); uerr != nil {
			return fmt.Errorf("couldn't clear the duplicate's account link: %w", uerr)
		}
	}

	// Through RemoveLadderEntrant, not a bare delete: it compacts positions so
	// the ladder stays a contiguous 1..N, which is the invariant the rest of the
	// ladder code relies on.
	return s.RemoveLadderEntrant(mergeID)
}

// ladderNameTaken reports whether this ladder already lists someone under
// [name], ignoring case and surrounding space.
//
// Used to WARN, never to refuse: two people at a club really can be called Chris
// B, and the organizer is the one who knows. What it prevents is the silent
// case — adding a second "Jen Whitfield" at 7pm on a Tuesday and only finding
// out weeks later, when the ladder has two of her with half a record each.
func (s *Service) ladderNameTaken(leagueBracketID, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	rows, err := s.sb.Select("ladder_entrants",
		"league_bracket_id=eq."+store.Q(leagueBracketID)+
			"&select=display_name&limit=1000")
	if err != nil {
		return false // never block an add because a check failed
	}
	for _, r := range rows {
		if strings.EqualFold(strings.TrimSpace(asStr(r, "display_name")), name) {
			return true
		}
	}
	return false
}
