package service

import (
	"sort"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// localDateOf renders an RFC3339 timestamp as YYYY-MM-DD in a timezone that is
// offsetMinutes from UTC. Postgres hands back both RFC3339 and RFC3339Nano
// depending on whether the value carries fractional seconds, so both are tried.
func localDateOf(ts string, offsetMinutes int) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if t, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return ""
		}
	}
	return t.UTC().Add(time.Duration(offsetMinutes) * time.Minute).Format("2006-01-02")
}

// Standings for a WINDOW OF TIME rather than the whole event.
//
// A perpetual league is one long-running event, so its all-time board answers
// "who is best this season" — but on the night itself the room wants "how did we
// do TONIGHT", and tomorrow that answer should be empty rather than yesterday's
// leftovers.
//
// The window is anchored on matches.completed_at — when a game was actually
// PLAYED — not on when its round was created. A schedule built the night before
// would otherwise file the whole session under the wrong day, and the live board
// would open empty on the day people are standing on the court.
//
// since/until are RFC3339 and half-open [since, until): the caller passes local
// midnight converted to UTC, so the day boundary follows whoever is looking
// rather than the server's timezone. Either may be empty for an open end.

// standingRowsBetween aggregates GP/W/L/PF/PA per player from completed pool
// matches PLAYED inside the window. Mirrors seasonStandingRows exactly, minus
// the round-created scoping, so a windowed board and the all-time board differ
// only in which games they count.
func (s *Service) standingRowsBetween(eventID, bracketID, since, until string) ([]model.Standing, error) {
	q := "event_id=eq." + store.Q(eventID) +
		"&stage=eq.pool&status=eq.completed" +
		"&select=team1_score,team2_score,winning_team,counts_for_diff," +
		"participants:match_participants(team,player:players!player_id(id,full_name))"
	if bracketID != "" {
		q += "&bracket_id=eq." + store.Q(bracketID)
	}
	if since != "" {
		q += "&completed_at=gte." + store.Q(since)
	}
	if until != "" {
		q += "&completed_at=lt." + store.Q(until)
	}
	rows, err := s.sb.SelectAll("matches", q)
	if err != nil {
		return nil, err
	}
	byPlayer := map[string]*model.Standing{}
	for _, r := range rows {
		// Same box-score convention as everywhere else: a bye is a completed
		// match with NULL scores (not a game); a forfeit fabricates a
		// target–0 with counts_for_diff=false (decides the match, but must not
		// inflate games played, points or differential).
		if asIntPtr(r, "team1_score") == nil || asIntPtr(r, "team2_score") == nil {
			continue
		}
		if cd := r["counts_for_diff"]; cd != nil && cd == false {
			continue
		}
		wt := asInt(r, "winning_team")
		t1, t2 := asInt(r, "team1_score"), asInt(r, "team2_score")
		parts, _ := r["participants"].([]any)
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			team := asInt(pm, "team")
			pl := asMap(pm, "player")
			pid := asStr(pl, "id")
			if pid == "" {
				continue
			}
			st := byPlayer[pid]
			if st == nil {
				st = &model.Standing{PlayerID: pid, FullName: asStr(pl, "full_name")}
				byPlayer[pid] = st
			}
			st.GamesPlayed++
			pf, pa := t2, t1
			if team == 1 {
				pf, pa = t1, t2
			}
			st.PointsFor += pf
			st.PointsAgainst += pa
			if wt == team {
				st.Wins++
			} else if wt == 1 || wt == 2 {
				st.Losses++
			}
		}
	}
	out := make([]model.Standing, 0, len(byPlayer))
	for _, st := range byPlayer {
		st.PointDiff = st.PointsFor - st.PointsAgainst
		out = append(out, *st)
	}
	return out, nil
}

// headToHeadBetween is headToHead scoped to the same window, so ties inside a
// day are broken by meetings THAT day — not by a result from three weeks ago
// that isn't on the board the viewer is reading.
func (s *Service) headToHeadBetween(eventID, bracketID, since, until string) (map[string]map[string]int, error) {
	q := "event_id=eq." + store.Q(eventID) +
		"&stage=eq.pool&status=eq.completed" +
		"&select=winning_team,counts_for_diff,participants:match_participants(player_id,team)"
	if bracketID != "" {
		q += "&bracket_id=eq." + store.Q(bracketID)
	}
	if since != "" {
		q += "&completed_at=gte." + store.Q(since)
	}
	if until != "" {
		q += "&completed_at=lt." + store.Q(until)
	}
	rows, err := s.sb.SelectAll("matches", q)
	if err != nil {
		return nil, err
	}
	h := map[string]map[string]int{}
	for _, r := range rows {
		wt := asInt(r, "winning_team")
		if wt != 1 && wt != 2 {
			continue
		}
		// Forfeit/walkover excluded from tiebreaks (see headToHead).
		if cd := r["counts_for_diff"]; cd != nil && cd == false {
			continue
		}
		parts, _ := r["participants"].([]any)
		winners, losers := []string{}, []string{}
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			pid := asStr(pm, "player_id")
			if pid == "" {
				continue
			}
			if asInt(pm, "team") == wt {
				winners = append(winners, pid)
			} else {
				losers = append(losers, pid)
			}
		}
		for _, w := range winners {
			for _, l := range losers {
				if h[w] == nil {
					h[w] = map[string]int{}
				}
				h[w][l]++
			}
		}
	}
	return h, nil
}

// StandingsBetween is Standings scoped to games played in [since, until).
// Ranked by the SAME rankStandings the all-time board uses, so it is the same
// leaderboard filtered — not a second one that orders ties differently.
func (s *Service) StandingsBetween(eventID, bracketID string, byWins bool, since, until string) ([]model.Standing, error) {
	out, err := s.standingRowsBetween(eventID, bracketID, since, until)
	if err != nil {
		return nil, err
	}
	h2h, _ := s.headToHeadBetween(eventID, bracketID, since, until)
	return rankStandings(out, h2h, byWins), nil
}

// SessionDay is one day this event actually played, for the Past Leaderboards
// list. Date is YYYY-MM-DD in the CALLER's timezone.
type SessionDay struct {
	Date  string `json:"date"`
	Games int    `json:"games"`
}

// SessionDays lists the days that have at least one played game, newest first.
//
// offsetMinutes is the caller's UTC offset, so "a day" means their local day —
// a 7pm session on the US west coast belongs to that evening, not to the
// following UTC date. Grouped in Go because the offset is per-request and
// PostgREST can't express the shifted date bucket.
func (s *Service) SessionDays(eventID, bracketID string, offsetMinutes int) ([]SessionDay, error) {
	q := "event_id=eq." + store.Q(eventID) +
		"&stage=eq.pool&status=eq.completed" +
		"&winning_team=not.is.null&select=completed_at"
	if bracketID != "" {
		q += "&bracket_id=eq." + store.Q(bracketID)
	}
	rows, err := s.sb.SelectAll("matches", q)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, r := range rows {
		d := localDateOf(asStr(r, "completed_at"), offsetMinutes)
		if d == "" {
			continue
		}
		count[d]++
	}
	out := make([]SessionDay, 0, len(count))
	for d, n := range count {
		out = append(out, SessionDay{Date: d, Games: n})
	}
	// Newest first: the most recent session is the one anyone opens this for.
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}
