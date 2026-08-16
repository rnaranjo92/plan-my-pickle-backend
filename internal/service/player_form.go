package service

import (
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// A player's recent form, for the chart on their own card.
//
// Read from match_participants — the same join that produces the GAMES figure —
// so the chart and the count can never disagree. The first version walked
// registrations and computed each event's standings instead, which needed an
// approved registration AND a past start date AND a standings row, and showed
// an empty chart for an account with games plainly recorded against it.
//
// POINT DIFFERENTIAL is what's plotted. A win count says how often; a
// differential says whether you were close, and a run of narrow losses looks
// nothing like a run of hidings. Being signed, it also gives the chart a zero
// line to read against rather than bar lengths to compare.

// maxFormMatches bounds the series. A card is a glance, not a season report.
const maxFormMatches = 12

// MyForm returns the caller's most recent completed matches, oldest first so a
// chart plots left to right.
func (s *Service) MyForm(userID, email string) ([]model.PlayerFormEntry, error) {
	if strings.TrimSpace(userID) == "" {
		return []model.PlayerFormEntry{}, nil
	}
	pls, err := s.sb.Select("players",
		"user_id=eq."+store.Q(userID)+"&select=id")
	if err != nil || len(pls) == 0 {
		return []model.PlayerFormEntry{}, nil
	}
	pids := make([]string, 0, len(pls))
	for _, pl := range pls {
		if id := asStr(pl, "id"); id != "" {
			pids = append(pids, id)
		}
	}
	if len(pids) == 0 {
		return []model.PlayerFormEntry{}, nil
	}

	// Which side the player was on decides the SIGN of their differential.
	parts, err := s.sb.Select("match_participants",
		"player_id="+store.In(pids)+"&select=match_id,team")
	if err != nil || len(parts) == 0 {
		return []model.PlayerFormEntry{}, nil
	}
	teamOf := make(map[string]int, len(parts))
	mids := make([]string, 0, len(parts))
	for _, pt := range parts {
		mid := asStr(pt, "match_id")
		if mid == "" {
			continue
		}
		if _, dup := teamOf[mid]; dup {
			continue
		}
		teamOf[mid] = asInt(pt, "team")
		mids = append(mids, mid)
	}
	if len(mids) == 0 {
		return []model.PlayerFormEntry{}, nil
	}

	rows, err := s.sb.Select("matches",
		"id="+store.In(mids)+"&status=eq.completed"+
			"&select=id,event_id,team1_score,team2_score,counts_for_diff,updated_at"+
			"&order=updated_at.desc&limit=200")
	if err != nil {
		return nil, err
	}

	type played struct {
		entry model.PlayerFormEntry
		at    string
	}
	list := make([]played, 0, len(rows))
	for _, m := range rows {
		t1, t2 := asIntPtr(m, "team1_score"), asIntPtr(m, "team2_score")
		if t1 == nil || t2 == nil {
			continue // a bye is not a game
		}
		// Same exclusion the games counter uses: a forfeit or walkover has a
		// score on paper that nobody played to.
		if cd := m["counts_for_diff"]; cd != nil && cd == false {
			continue
		}
		mid := asStr(m, "id")
		mine, theirs := *t1, *t2
		if teamOf[mid] == 2 {
			mine, theirs = *t2, *t1
		}
		won := 0
		lost := 1
		if mine > theirs {
			won, lost = 1, 0
		}
		list = append(list, played{
			at: asStr(m, "updated_at"),
			entry: model.PlayerFormEntry{
				EventID:     asStr(m, "event_id"),
				PlayedAt:    asStr(m, "updated_at"),
				GamesPlayed: 1,
				Wins:        won,
				Losses:      lost,
				PointDiff:   mine - theirs,
			},
		})
		if len(list) >= maxFormMatches {
			break
		}
	}
	if len(list) == 0 {
		return []model.PlayerFormEntry{}, nil
	}

	// Name the events in one batched read, so a bar can say where it came from.
	ids := make([]string, 0, len(list))
	for _, p := range list {
		if p.entry.EventID != "" {
			ids = append(ids, p.entry.EventID)
		}
	}
	if len(ids) > 0 {
		if evs, err := s.sb.Select("events",
			"id="+store.In(ids)+"&select=id,name"); err == nil {
			byID := make(map[string]string, len(evs))
			for _, e := range evs {
				byID[asStr(e, "id")] = asStr(e, "name")
			}
			for i := range list {
				list[i].entry.EventName = byID[list[i].entry.EventID]
			}
		}
	}

	// Oldest first for the chart. The query returned newest first so the cap
	// keeps RECENT matches; flipping here is what makes it read left to right.
	sort.SliceStable(list, func(i, j int) bool { return list[i].at < list[j].at })
	out := make([]model.PlayerFormEntry, 0, len(list))
	for _, p := range list {
		out = append(out, p.entry)
	}
	return out, nil
}
