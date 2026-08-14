package service

import (
	"log"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// syncLadderRegistrations mirrors a ladder's entrants onto its ongoing event as
// registrations, so the ladder has ONE roster instead of two.
//
// A ladder's players live in ladder_entrants (keyed to the division). Everything
// an event offers — the Players tab, approvals, check-in, finances/dues — reads
// registrations (keyed to the event). Nothing ever wrote the second one, so a
// ladder with a full roster showed "No players yet" on its own Players tab, and
// dues had nothing to charge. The event is what those features hang off; the
// roster just never followed it there.
//
// Idempotent and self-healing:
//   - an entrant already carrying a player_id that's registered is skipped;
//   - an UNLINKED entrant (a typed-in name with no account) is registered via
//     the normal organizer-add path, and the new player id is written BACK onto
//     the entrant — so the next pass dedupes by id instead of by name, and the
//     two rosters converge on the same person rather than drifting.
//
// Best-effort throughout: this runs on a read path, and a ladder that can't sync
// must still open. Failures are logged, never returned.
func (s *Service) syncLadderRegistrations(eventID string, brackets []model.LeagueBracket) {
	if eventID == "" || len(brackets) == 0 {
		return
	}
	// Who's already registered, by player id.
	regRows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&select=player_id")
	if err != nil {
		log.Printf("ladder sync: could not read registrations for event %s: %v",
			eventID, err)
		return
	}
	registered := make(map[string]bool, len(regRows))
	for _, r := range regRows {
		if pid := asStr(r, "player_id"); pid != "" {
			registered[pid] = true
		}
	}
	// Names already on the event, so an unlinked entrant isn't added twice when
	// the same person was registered directly. Only needed if some entrant is
	// unlinked, so it's loaded lazily below.
	var regNames map[string]bool

	// The event's own divisions, matched to the league's by name — that's how
	// provisionPerpetualLeagueEvent created them.
	evBrackets, err := s.sb.Select("brackets",
		"event_id=eq."+store.Q(eventID)+"&select=id,name")
	if err != nil {
		log.Printf("ladder sync: could not read event brackets for %s: %v",
			eventID, err)
		return
	}
	bracketByName := make(map[string]string, len(evBrackets))
	for _, b := range evBrackets {
		bracketByName[normalizeName(asStr(b, "name"))] = asStr(b, "id")
	}

	for _, div := range brackets {
		entrants, err := s.ListLadder(div.ID)
		if err != nil {
			log.Printf("ladder sync: could not list division %s: %v", div.ID, err)
			continue
		}
		targetBracket := bracketByName[normalizeName(div.Name)]
		for _, e := range entrants {
			// A TEAM entrant is a pair sharing one rung, not a person. Registering
			// it would put a team name in a list of players.
			if e.IsTeam {
				continue
			}
			if e.PlayerID != nil && *e.PlayerID != "" {
				if registered[*e.PlayerID] {
					continue
				}
				row := map[string]any{
					"event_id":       eventID,
					"player_id":      *e.PlayerID,
					"check_in_token": newID(),
				}
				if targetBracket != "" {
					row["bracket_id"] = targetBracket
				}
				if _, err := s.sb.Insert("registrations", row); err != nil {
					log.Printf("ladder sync: could not register player %s on %s: %v",
						*e.PlayerID, eventID, err)
					continue
				}
				registered[*e.PlayerID] = true
				continue
			}
			// Unlinked entrant — a name someone typed in. Load the registered
			// names once so we don't add a second row for a person who already
			// registered directly.
			name := strings.TrimSpace(e.DisplayName)
			if name == "" {
				continue
			}
			if regNames == nil {
				regNames = s.registeredNames(eventID)
			}
			if regNames[normalizeName(name)] {
				continue
			}
			reg, err := s.RegisterPlayer(eventID, model.RegisterRequest{
				TrustedAdd: true, // organizer-side add: skip the public anti-dummy gates
				// AllowNoContact, because two product rules disagree here and
				// this is the only place they meet: leagues require a phone or
				// email ("they have to be reachable between sessions"), while a
				// ladder's own "Add a name" deliberately creates entries with
				// neither. Enforcing the league rule during a MIRROR wouldn't
				// make anyone reachable — the entrant already exists on the
				// ladder either way — it would just refuse every typed-in name
				// and leave the two rosters permanently disagreeing, which is
				// the bug this whole function exists to fix.
				//
				// The rule still belongs at the point of ENTRY. If ladder names
				// should require contact details, "Add a name" is where to ask.
				AllowNoContact: true,
				FullName:       name,
				BracketID:      targetBracket,
			}, "")
			if err != nil {
				log.Printf("ladder sync: could not register %q on %s: %v",
					name, eventID, err)
				continue
			}
			regNames[normalizeName(name)] = true
			if reg.PlayerID != "" {
				registered[reg.PlayerID] = true
				// Link the entrant to the player we just created. Without this the
				// name match is the only thing keeping the two rosters together,
				// and a rename on either side would quietly duplicate the person.
				if _, err := s.sb.Update("ladder_entrants",
					"id=eq."+store.Q(e.ID),
					map[string]any{"player_id": reg.PlayerID}); err != nil {
					log.Printf("ladder sync: could not link entrant %s to player %s: %v",
						e.ID, reg.PlayerID, err)
				}
			}
		}
	}
}

// registeredNames returns the normalized full names already registered on an
// event, for matching entrants that carry no player id.
func (s *Service) registeredNames(eventID string) map[string]bool {
	out := map[string]bool{}
	rows, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&select=player_id")
	if err != nil || len(rows) == 0 {
		return out
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if pid := asStr(r, "player_id"); pid != "" {
			ids = append(ids, pid)
		}
	}
	if len(ids) == 0 {
		return out
	}
	prows, err := s.sb.Select("players", "id="+store.In(ids)+"&select=full_name")
	if err != nil {
		return out
	}
	for _, p := range prows {
		if n := asStr(p, "full_name"); n != "" {
			out[normalizeName(n)] = true
		}
	}
	return out
}

// normalizeName folds case and collapses whitespace so "ada  chen" and
// "Ada Chen" are the same person for dedupe purposes.
func normalizeName(n string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(n))), " ")
}
