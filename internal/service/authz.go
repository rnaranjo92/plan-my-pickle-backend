package service

import (
	"fmt"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// ownerKindTable maps an authz resource kind to the table that carries its
// event_id. "event" is handled specially (the id IS the event).
var ownerKindTable = map[string]string{
	"match":        "matches",
	"bracket":      "brackets",
	"round":        "rounds",
	"finance":      "finance_entries",
	"checklist":    "checklist_items",
	"registration": "registrations",
	"feed_item":    "feed_items",
}

// OwnerOf returns the auth-user id (events.owner_id) of the event behind a
// resource, so a handler can verify the caller owns it before mutating.
//
// kind is "event", "league", or one of match|bracket|round|finance|checklist;
// id is that resource's id. Returns ErrNotFound if the resource (or its event)
// is missing, and "" with a nil error when the event has no owner (legacy/unowned
// events) — callers should treat an empty owner as "nobody may mutate it".
func (s *Service) OwnerOf(kind, id string) (string, error) {
	// A league carries owner_id directly (it has no event_id), so resolve it on
	// its own table rather than walking through events.
	if kind == "league" {
		row, err := s.sb.SelectOne("leagues", "id=eq."+store.Q(id)+"&select=owner_id")
		if err != nil {
			return "", err
		}
		if row == nil {
			return "", ErrNotFound
		}
		return asStr(row, "owner_id"), nil
	}
	eventID := id
	if kind != "event" {
		table, ok := ownerKindTable[kind]
		if !ok {
			return "", fmt.Errorf("OwnerOf: unknown kind %q", kind)
		}
		row, err := s.sb.SelectOne(table, "id=eq."+store.Q(id)+"&select=event_id")
		if err != nil {
			return "", err
		}
		if row == nil {
			return "", ErrNotFound
		}
		eventID = asStr(row, "event_id")
	}
	ev, err := s.sb.SelectOne("events", "id=eq."+store.Q(eventID)+"&select=owner_id")
	if err != nil {
		return "", err
	}
	if ev == nil {
		return "", ErrNotFound
	}
	return asStr(ev, "owner_id"), nil
}

// LeagueIDOfDivision returns the league id behind a division (league_bracket),
// or ErrNotFound if the division is missing. Used by the leagueViewer gate to
// authorize ladder/team/flex READS keyed on a division id: it maps the division
// to its league so ownership/participation can be checked against that league.
func (s *Service) LeagueIDOfDivision(leagueBracketID string) (string, error) {
	row, err := s.sb.SelectOne("league_brackets",
		"id=eq."+store.Q(leagueBracketID)+"&select=league_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return asStr(row, "league_id"), nil
}

// EventIDOfMatch returns the event_id a match belongs to, or ErrNotFound if the
// match is missing. Used by the scorekeeper auth path (ownerOrPasscode) to map a
// match to the event whose admin passcode it must validate.
func (s *Service) EventIDOfMatch(matchID string) (string, error) {
	row, err := s.sb.SelectOne("matches", "id=eq."+store.Q(matchID)+"&select=event_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return asStr(row, "event_id"), nil
}
