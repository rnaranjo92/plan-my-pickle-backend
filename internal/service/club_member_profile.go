package service

import (
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// The member profile an ADMIN opens from the club's Members tab: who this
// person is, how to reach them, and what they've actually done here.
//
// ADMIN-ONLY, deliberately. Phone and email are the obvious reason, but the
// gate is structural too: the public members list blanks even the user id
// (anti-harvest), so only an admin ever holds the key this endpoint is asked
// with. The record it shows is CLUB-scoped — their wins here, their nights
// here — because "who is this person in my club" is the question the sheet
// answers; their life on the wider platform belongs to their own profile.

// ClubMemberEventRow is one event of theirs in this club, for the history list.
type ClubMemberEventRow struct {
	EventID  string `json:"eventId"`
	Name     string `json:"name"`
	StartsAt string `json:"startsAt,omitempty"`
}

// ClubMemberProfile is the sheet's payload.
type ClubMemberProfile struct {
	UserID   string `json:"userId"`
	FullName string `json:"fullName"`
	PhotoURL string `json:"photoUrl,omitempty"`
	Role     string `json:"role"`
	JoinedAt string `json:"joinedAt,omitempty"`
	// Contact — the reason this endpoint is admin-gated.
	Phone string `json:"phone,omitempty"`
	Email string `json:"email,omitempty"`
	// Ratings.
	DuprID        string   `json:"duprId,omitempty"`
	DoublesRating *float64 `json:"doublesRating,omitempty"`
	// The club-scoped record.
	GamesPlayed  int `json:"gamesPlayed"`
	Wins         int `json:"wins"`
	Losses       int `json:"losses"`
	EventsPlayed int `json:"eventsPlayed"`
	// Everything they organize on the platform, not just here — an admin
	// deciding whether to hand someone the Tuesday league wants to know if
	// they've ever run anything at all.
	EventsOrganized int `json:"eventsOrganized"`
	// Swing — the last few results oldest→newest, true = win. The same strip
	// the player's own ID card draws, computed over THIS club's matches.
	Swing []bool `json:"swing"`
	// Recent events here, newest first.
	History []ClubMemberEventRow `json:"history"`
}

// ClubMemberProfileFor assembles the sheet. Caller must run the club.
func (s *Service) ClubMemberProfileFor(
	clubID, targetUserID, callerID string,
) (ClubMemberProfile, error) {
	out := ClubMemberProfile{
		UserID:  targetUserID,
		Swing:   []bool{},
		History: []ClubMemberEventRow{},
	}
	if !s.IsClubAdmin(clubID, callerID) {
		return out, ErrForbidden
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return out, ErrNotFound
	}

	// Role + joined date. The owner has no member row; everyone else does.
	club, err := s.sb.SelectOne("clubs",
		"id=eq."+store.Q(clubID)+"&select=owner_id")
	if err != nil || club == nil {
		return out, ErrNotFound
	}
	if asStr(club, "owner_id") == targetUserID {
		out.Role = "owner"
	} else if m, merr := s.sb.SelectOne("club_members",
		"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(targetUserID)+
			"&select=role,created_at"); merr == nil && m != nil {
		out.Role = strings.TrimSpace(asStr(m, "role"))
		out.JoinedAt = asStr(m, "created_at")
	} else {
		return out, ErrNotFound // not a member of this club
	}

	out.FullName = s.resolveDisplayName(targetUserID, "")
	if pr, perr := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(targetUserID)+"&select=photo_url"); perr == nil &&
		pr != nil {
		out.PhotoURL = asStr(pr, "photo_url")
	}

	// Contact + DUPR from their player rows (ordered, so the sheet doesn't
	// show a different number each open) and the DUPR connection when linked.
	if rows, rerr := s.sb.Select("players",
		"user_id=eq."+store.Q(targetUserID)+
			"&select=id,email,phone,dupr_id,dupr_rating&order=id.asc"); rerr == nil {
		for _, r := range rows {
			if out.Email == "" {
				out.Email = strings.TrimSpace(asStr(r, "email"))
			}
			if out.Phone == "" {
				out.Phone = strings.TrimSpace(asStr(r, "phone"))
			}
			if out.DuprID == "" {
				out.DuprID = strings.TrimSpace(asStr(r, "dupr_id"))
			}
			if out.DoublesRating == nil {
				out.DoublesRating = asFloatPtr(r, "dupr_rating")
			}
		}
	}
	if c, cerr := s.sb.SelectOne("dupr_connections",
		"user_id=eq."+store.Q(targetUserID)+
			"&select=dupr_id,doubles_rating"); cerr == nil && c != nil {
		if id := asStr(c, "dupr_id"); id != "" {
			out.DuprID = id
		}
		if r := asFloatPtr(c, "doubles_rating"); r != nil {
			out.DoublesRating = r
		}
	}

	out.EventsOrganized = s.countRows("events",
		"owner_id=eq."+store.Q(targetUserID), "id")

	// Everything below is scoped to THIS club's events.
	events, eerr := s.ClubEvents(clubID)
	if eerr != nil || len(events) == 0 {
		return out, nil
	}
	eventIDs := make([]string, 0, len(events))
	nameByEvent := make(map[string]string, len(events))
	startByEvent := make(map[string]string, len(events))
	for _, e := range events {
		eventIDs = append(eventIDs, e.ID)
		nameByEvent[e.ID] = e.Name
		if e.StartsAt != nil {
			startByEvent[e.ID] = *e.StartsAt
		}
	}

	pids, perr := s.playerIDsForUser(targetUserID, "")
	if perr != nil || len(pids) == 0 {
		return out, nil
	}

	// History: their registrations in club events, newest first, one row per
	// event however many divisions they entered.
	if regs, rerr := s.sb.Select("registrations",
		"player_id="+store.In(pids)+"&event_id="+store.In(eventIDs)+
			"&select=event_id&order=created_at.desc"); rerr == nil {
		seen := map[string]bool{}
		for _, r := range regs {
			eid := asStr(r, "event_id")
			if eid == "" || seen[eid] {
				continue
			}
			seen[eid] = true
			if len(out.History) < 10 {
				out.History = append(out.History, ClubMemberEventRow{
					EventID:  eid,
					Name:     nameByEvent[eid],
					StartsAt: startByEvent[eid],
				})
			}
		}
		out.EventsPlayed = len(seen)
	}

	// The club-scoped box score + the swing strip, using the same real-games
	// rules as PlayerProfile: byes have no score, and fabricated
	// walkover/forfeit scores (counts_for_diff=false) don't count.
	parts, perr2 := s.sb.Select("match_participants",
		"player_id="+store.In(pids)+"&select=team,match_id")
	if perr2 != nil || len(parts) == 0 {
		return out, nil
	}
	teamByMatch := map[string]int{}
	mids := make([]string, 0, len(parts))
	for _, p := range parts {
		if mid := asStr(p, "match_id"); mid != "" {
			teamByMatch[mid] = asInt(p, "team")
			mids = append(mids, mid)
		}
	}
	type result struct {
		at  string
		won bool
	}
	results := []result{}
	if mrows, merr := s.sb.Select("matches",
		"id="+store.In(mids)+"&event_id="+store.In(eventIDs)+
			"&status=eq.completed"+
			"&select=id,team1_score,team2_score,winning_team,counts_for_diff,"+
			"created_at"); merr == nil {
		for _, m := range mrows {
			t1, t2 := asIntPtr(m, "team1_score"), asIntPtr(m, "team2_score")
			if t1 == nil || t2 == nil {
				continue
			}
			if cd := m["counts_for_diff"]; cd != nil && cd == false {
				continue
			}
			team := teamByMatch[asStr(m, "id")]
			won := false
			if wt := asIntPtr(m, "winning_team"); wt != nil && *wt == team {
				won = true
			}
			out.GamesPlayed++
			if won {
				out.Wins++
			} else {
				out.Losses++
			}
			results = append(results, result{at: asStr(m, "created_at"), won: won})
		}
	}
	// Oldest→newest so the strip reads left to right, capped to the latest 10.
	sort.Slice(results, func(i, j int) bool { return results[i].at < results[j].at })
	if len(results) > 10 {
		results = results[len(results)-10:]
	}
	for _, r := range results {
		out.Swing = append(out.Swing, r.won)
	}
	return out, nil
}
