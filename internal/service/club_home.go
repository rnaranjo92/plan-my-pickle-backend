package service

import (
	"sort"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// What a member sees when they open their club.
//
// The club page listed members, events and an all-time leaderboard — a filing
// cabinet. Accurate, and no reason to open it on a Tuesday. A member arrives
// with two questions, neither of which it answered: WHAT'S ON, and HOW AM I
// DOING. Both are aggregations over data already stored; nothing new is
// recorded to produce them.

// ClubHome is the answer to both questions, for one viewer.
type ClubHome struct {
	// Upcoming is the club's next few sessions, soonest first. Empty when
	// nothing is scheduled — which is itself worth showing, because a club with
	// nothing on is a club that needs someone to put something on.
	Upcoming []model.Event `json:"upcoming"`
	// Me is the viewer's own record in this club. Nil when they are signed out,
	// or have played nothing here yet.
	Me *ClubMemberRecord `json:"me,omitempty"`
	// Members and MembersPlaying give the club a sense of itself: how many are
	// in it, and how many have actually played.
	Members        int `json:"members"`
	MembersPlaying int `json:"membersPlaying"`
	// Attendance is the viewer's recent turnout — "6 of the last 8". Nil when
	// signed out, or when the club has never run a dated session.
	Attendance *ClubAttendance `json:"attendance,omitempty"`
}

// ClubMemberRecord is one person's standing in their own club.
//
// Rank is included because "12 wins" means nothing on its own and "9th of 34"
// means something immediately — and because a member reading their own line is
// the moment the leaderboard stops being a corporate artefact.
type ClubMemberRecord struct {
	Name         string  `json:"name"`
	Rank         int     `json:"rank"`
	OfPlayers    int     `json:"ofPlayers"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	GamesPlayed  int     `json:"gamesPlayed"`
	WinPct       float64 `json:"winPct"`
	EventsPlayed int     `json:"eventsPlayed"`
	PointDiff    int     `json:"pointDiff"`
}

// upcomingClubEvents returns the events starting from [now] onward, soonest
// first, capped at [limit].
//
// Pure, so "what counts as upcoming" is testable without a club. An event with
// no start date is NOT upcoming: a ladder has no date by design, and putting it
// under "what's on next" would tell a member to turn up for something that
// isn't happening on any particular evening.
func upcomingClubEvents(events []model.Event, now time.Time, limit int) []model.Event {
	type dated struct {
		ev model.Event
		at time.Time
	}
	var out []dated
	for _, ev := range events {
		if ev.StartsAt == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(*ev.StartsAt))
		if err != nil {
			continue
		}
		// A session that started earlier TODAY still counts as on: people are
		// mid-way through it, and dropping it at the start time would empty the
		// card exactly when the club is busiest.
		if at.Before(now.Add(-12 * time.Hour)) {
			continue
		}
		out = append(out, dated{ev: ev, at: at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	res := make([]model.Event, 0, len(out))
	for _, d := range out {
		res = append(res, d.ev)
	}
	return res
}

// ClubHomeFor builds the club's home view for [viewerID] ("" when signed out).
func (s *Service) ClubHomeFor(clubID, viewerID string) (ClubHome, error) {
	out := ClubHome{Upcoming: []model.Event{}}

	// Confirm the club EXISTS first. Without this a deleted or mistyped id
	// answered 200 with an empty home — a page that renders "nothing on" for a
	// club that isn't there, which reads as a dead club rather than a wrong
	// link and is the harder of the two to debug.
	if row, cerr := s.sb.SelectOne("clubs",
		"id=eq."+store.Q(clubID)+"&select=id"); cerr != nil {
		return out, cerr
	} else if row == nil {
		return out, ErrNotFound
	}

	events, err := s.ClubEventsFor(clubID, viewerID)
	if err != nil {
		return out, err
	}
	out.Upcoming = upcomingClubEvents(events, time.Now(), 3)

	// The leaderboard is already the club's record of itself, so the viewer's
	// line is a lookup in it rather than a second way of counting — two
	// aggregations of the same games that could disagree is how a member ends up
	// reading two different truths about their own season.
	board, berr := s.ClubLeaderboard(clubID)
	if berr == nil {
		out.MembersPlaying = len(board)
		if rec := s.myClubRecord(board, viewerID); rec != nil {
			out.Me = rec
		}
	}
	// Turning up is worth recognising on its own. The leaderboard rewards
	// winning, which not everybody can do — the member who has been there every
	// Tuesday since March and loses more than they win deserves a line that
	// isn't evidence they don't belong.
	out.Attendance = s.clubAttendanceFor(events, viewerID)
	out.Members = s.countRows("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id", "user_id")
	return out, nil
}

// myClubRecord finds the viewer's row in an already-computed leaderboard.
//
// Matched on the NAME the leaderboard chose for them, resolved from the
// viewer's account — the leaderboard merges by account internally and then
// presents names, so this is the same person by construction. Returns nil
// rather than a zeroed row when they haven't played: "0 wins, rank 34th" reads
// as a bad record, when the truth is no record yet.
func (s *Service) myClubRecord(
	board []model.ClubStanding, viewerID string,
) *ClubMemberRecord {
	if strings.TrimSpace(viewerID) == "" || len(board) == 0 {
		return nil
	}
	// Prefer the ACCOUNT key: two members with the same display name must not
	// read each other's stats. The leaderboard keys rows by "account:<uid>"
	// when the player has an account, so build the viewer's key the same way
	// and match on it; fall back to name only when neither side has an account.
	wantKey := ""
	if pids, _ := s.playerIDsForUser(viewerID, ""); len(pids) > 0 {
		for _, uid := range s.accountsForPlayers(pids) {
			if uid != "" {
				wantKey = "account:" + uid
				break
			}
		}
	}
	name := strings.ToLower(strings.TrimSpace(s.nameOfUser(viewerID)))
	if wantKey == "" && name == "" {
		return nil
	}
	for i, row := range board {
		if wantKey != "" {
			if row.IdentityKey != wantKey {
				continue
			}
		} else if strings.ToLower(strings.TrimSpace(row.Name)) != name {
			continue
		}
		return &ClubMemberRecord{
			Name:         row.Name,
			Rank:         i + 1,
			OfPlayers:    len(board),
			Wins:         row.Wins,
			Losses:       row.Losses,
			GamesPlayed:  row.GamesPlayed,
			WinPct:       winPct(row.Wins, row.GamesPlayed),
			EventsPlayed: row.EventsPlayed,
			PointDiff:    row.PointDiff,
		}
	}
	return nil
}
