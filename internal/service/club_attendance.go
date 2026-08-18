package service

import (
	"sort"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Turning up.
//
// A club is held together by the people who come most weeks, and nothing in the
// product noticed them. The leaderboard rewards winning, which is a different
// thing and not one everybody can do — the member who has been there every
// Tuesday since March and loses more than they win reads their own line as
// evidence they don't belong.
//
// Attendance is measured from REGISTRATIONS rather than results, deliberately.
// Signing up and turning out is the thing being recognised; whether the games
// went well is what the rest of the page is about. It also means someone who
// came and sat out an injury still counts, which is correct — they came.

// clubAttendanceWindow is how many recent sessions "lately" means.
//
// Eight is about two months of weekly play: long enough that one missed week
// doesn't erase somebody, short enough that it describes now rather than a
// history. A number here is also why the copy can say "6 of the last 8" instead
// of a percentage, which nobody pictures.
const clubAttendanceWindow = 8

// ClubAttendance is one member's recent turnout.
type ClubAttendance struct {
	// Played and Of are the headline: "6 of the last 8".
	Played int `json:"played"`
	Of     int `json:"of"`
	// Streak is consecutive sessions attended, counting back from the most
	// recent one that has happened. 0 when they missed the last one.
	Streak int `json:"streak"`
}

// attendanceFrom computes the window from a club's dated events and the set of
// event ids a member appears in.
//
// Pure, so the counting rule is testable without a club: what counts as a
// session, which end the streak counts from, and what happens when a club has
// played fewer than a full window.
func attendanceFrom(
	events []model.Event, attended map[string]bool, now time.Time, window int,
) ClubAttendance {
	type dated struct {
		id string
		at time.Time
	}
	var past []dated
	for _, ev := range events {
		if ev.StartsAt == nil {
			continue // a ladder has no session date; it isn't a Tuesday
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(*ev.StartsAt))
		if err != nil || at.After(now) {
			// Only sessions that have HAPPENED. Counting a scheduled one as
			// missed would tell somebody they'd skipped next Tuesday.
			continue
		}
		past = append(past, dated{id: ev.ID, at: at})
	}
	// Most recent first — the streak counts back from the last session played.
	sort.SliceStable(past, func(i, j int) bool { return past[i].at.After(past[j].at) })
	if window > 0 && len(past) > window {
		past = past[:window]
	}

	out := ClubAttendance{Of: len(past)}
	counting := true
	for _, d := range past {
		if attended[d.id] {
			out.Played++
			if counting {
				out.Streak++
			}
			continue
		}
		counting = false // a missed session ends the streak, not the count
	}
	return out
}

// clubAttendanceFor returns [viewerID]'s turnout at this club's recent
// sessions, or nil when there is nothing to say.
//
// Best-effort: attendance is a flourish on a page that already works, so every
// failure here returns nil and the card simply doesn't mention it. Two small
// queries, not a traversal of every event's results — this is "did they sign
// up", which registrations answer directly.
func (s *Service) clubAttendanceFor(
	events []model.Event, viewerID string,
) *ClubAttendance {
	if strings.TrimSpace(viewerID) == "" || len(events) == 0 {
		return nil
	}
	// A person can hold more than one player row (registered by phone once and
	// by email another time), so match on ALL of them or their attendance is
	// split the same way the leaderboard's used to be.
	prows, err := s.sb.Select("players",
		"user_id=eq."+store.Q(viewerID)+"&select=id&limit=50")
	if err != nil || len(prows) == 0 {
		return nil
	}
	playerIDs := make([]string, 0, len(prows))
	for _, r := range prows {
		if id := strings.TrimSpace(asStr(r, "id")); id != "" {
			playerIDs = append(playerIDs, id)
		}
	}
	eventIDs := make([]string, 0, len(events))
	for _, ev := range events {
		if ev.StartsAt != nil {
			eventIDs = append(eventIDs, ev.ID)
		}
	}
	if len(playerIDs) == 0 || len(eventIDs) == 0 {
		return nil
	}
	regs, err := s.sb.SelectAll("registrations",
		"player_id="+store.In(playerIDs)+
			"&event_id="+store.In(eventIDs)+
			"&select=event_id")
	if err != nil {
		return nil
	}
	attended := map[string]bool{}
	for _, r := range regs {
		attended[asStr(r, "event_id")] = true
	}
	at := attendanceFrom(events, attended, time.Now(), clubAttendanceWindow)
	if at.Of == 0 {
		return nil // the club has never run a dated session; nothing to report
	}
	return &at
}
