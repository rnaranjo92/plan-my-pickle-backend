package service

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// The view a club owner opens every week.
//
// The members list was a directory: name, photo, role. It answers "who is in
// this club" and nothing about the only question an owner actually has, which
// is WHO IS SLIPPING AWAY. A member leaves a club by degrees — one missed
// Tuesday, then three, then they're gone and nobody noticed the moment a text
// would have brought them back.
//
// Deliberately about TURNING UP, not winning. The leaderboard already covers
// winning, and a roster sorted by wins would tell an owner to chase their best
// players, who are the least likely to leave.

// ClubRosterEntry is one member, and how they're doing at coming along.
type ClubRosterEntry struct {
	UserID   string `json:"userId"`
	FullName string `json:"fullName,omitempty"`
	PhotoURL string `json:"photoUrl,omitempty"`
	Role     string `json:"role,omitempty"`

	// Played / Of are their turnout over the recent window ("3 of the last 8").
	Played int `json:"played"`
	Of     int `json:"of"`
	// MissedRun is how many of the club's most recent sessions they have missed
	// in a row. 0 means they were at the last one.
	MissedRun int `json:"missedRun"`
	// LastPlayed is the date of the last session they came to (RFC3339), "" if
	// they never have.
	LastPlayed string `json:"lastPlayed,omitempty"`
	// Status is the one word an owner scans for — see clubMemberStatus.
	Status string `json:"status"`
	// DuesPaid is meaningful only while a club is collecting; DuesTracked says
	// whether it is, so the UI can stay silent about money for the clubs that
	// don't charge — which is most of them.
	DuesTracked bool `json:"duesTracked"`
	DuesPaid    bool `json:"duesPaid"`
}

// Status values. Words an owner would use, not states a database would.
const (
	ClubStatusActive     = "active"
	ClubStatusSlipping   = "slipping"
	ClubStatusLapsed     = "lapsed"
	ClubStatusNeverAlong = "never_played"
)

// clubMemberStatus turns a run of missed sessions into the word an owner acts
// on. Pure, because where the lines fall is a judgement worth testing.
//
// The thresholds assume roughly weekly play, which is what a club night is:
//
//	0-2 missed  active     — a normal life. Nobody is at everything.
//	3-5 missed  slipping   — about a month away. This is the ONLY one that is
//	                         actionable, and the entire reason for this view:
//	                         a message here brings people back, and the same
//	                         message a month later reads as an accusation.
//	6+  missed  lapsed     — gone for now. Worth a season invite, not a nudge.
//
// Someone who joined and never came is NOT lapsed. They are a different problem
// with a different fix — the club never got them through the door once — and
// lumping them in hides both.
func clubMemberStatus(everPlayed bool, missedRun int) string {
	if !everPlayed {
		return ClubStatusNeverAlong
	}
	switch {
	case missedRun <= 2:
		return ClubStatusActive
	case missedRun <= 5:
		return ClubStatusSlipping
	default:
		return ClubStatusLapsed
	}
}

// rosterOrder ranks entries so the people who need attention are at the top.
//
// SLIPPING first, deliberately — not lapsed, and not the most active. The list
// is a to-do, and the top of it should be the members a message would still
// reach in time. Sorting by "worst first" would put the long-gone above the
// nearly-gone and bury the only people an owner can still do something about.
func rosterOrder(status string) int {
	switch status {
	case ClubStatusSlipping:
		return 0
	case ClubStatusNeverAlong:
		return 1
	case ClubStatusLapsed:
		return 2
	default:
		return 3
	}
}

// ClubRoster lists every member with their recent turnout. Owner or co-owner:
// this is management information — who is drifting away is not a fact a club
// should publish about its own members.
func (s *Service) ClubRoster(clubID, callerID string) ([]ClubRosterEntry, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return nil, err
	}
	members, err := s.ClubMembers(clubID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return []ClubRosterEntry{}, nil
	}
	events, err := s.ClubEvents(clubID)
	if err != nil {
		return nil, err
	}
	sessions := pastSessions(events, time.Now(), clubAttendanceWindow)

	// user → their player rows, and player → user, in one query. A person can
	// hold several player rows (registered by phone once, by email another
	// time); missing that would split their attendance the way the leaderboard
	// used to split their record.
	uids := make([]string, 0, len(members))
	for _, m := range members {
		if m.UserID != "" {
			uids = append(uids, m.UserID)
		}
	}
	playerToUser := map[string]string{}
	if len(uids) > 0 {
		if prows, perr := s.sb.SelectAll("players",
			"user_id="+store.In(uids)+"&select=id,user_id"); perr == nil {
			for _, r := range prows {
				if id := asStr(r, "id"); id != "" {
					playerToUser[id] = asStr(r, "user_id")
				}
			}
		}
	}

	// Which users were at which session.
	attended := map[string]map[string]bool{} // userID -> eventID -> true
	if len(sessions) > 0 && len(playerToUser) > 0 {
		pids := make([]string, 0, len(playerToUser))
		for pid := range playerToUser {
			pids = append(pids, pid)
		}
		eids := make([]string, 0, len(sessions))
		for _, sess := range sessions {
			eids = append(eids, sess.id)
		}
		if regs, rerr := s.sb.SelectAll("registrations",
			"player_id="+store.In(pids)+"&event_id="+store.In(eids)+
				"&select=player_id,event_id"); rerr == nil {
			for _, r := range regs {
				uid := playerToUser[asStr(r, "player_id")]
				if uid == "" {
					continue
				}
				if attended[uid] == nil {
					attended[uid] = map[string]bool{}
				}
				attended[uid][asStr(r, "event_id")] = true
			}
		}
	}

	// Dues, if this club collects them. One query for the whole roster.
	period := s.OpenDuesPeriod(clubID)
	paid := map[string]bool{}
	if period != nil {
		paid = s.duesPaidUserIDs(period.ID)
	}

	out := make([]ClubRosterEntry, 0, len(members))
	for _, m := range members {
		e := ClubRosterEntry{
			UserID:   m.UserID,
			FullName: m.FullName,
			PhotoURL: m.PhotoURL,
			Role:     m.Role,
			Of:       len(sessions),
		}
		mine := attended[m.UserID]
		counting := true
		for _, sess := range sessions { // most recent first
			if mine[sess.id] {
				e.Played++
				counting = false
				if e.LastPlayed == "" {
					e.LastPlayed = sess.at.UTC().Format(time.RFC3339)
				}
				continue
			}
			if counting {
				e.MissedRun++
			}
		}
		e.Status = clubMemberStatus(e.Played > 0, e.MissedRun)
		e.DuesTracked = period != nil
		e.DuesPaid = paid[m.UserID]
		out = append(out, e)
	}

	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := rosterOrder(out[i].Status), rosterOrder(out[j].Status)
		if oi != oj {
			return oi < oj
		}
		// Within a group, the longest away first — they are the closest to
		// being gone.
		if out[i].MissedRun != out[j].MissedRun {
			return out[i].MissedRun > out[j].MissedRun
		}
		return strings.ToLower(out[i].FullName) < strings.ToLower(out[j].FullName)
	})
	return out, nil
}

// clubSession is one of the club's dated sessions.
type clubSession struct {
	id string
	at time.Time
}

// pastSessions returns the club's most recent sessions that have HAPPENED,
// newest first, capped at [window].
//
// Shared by the roster and the member's own attendance card so the two always
// describe the same set of Tuesdays — an owner and a member reading different
// windows would disagree about whether somebody had been turning up.
func pastSessions(events []model.Event, now time.Time, window int) []clubSession {
	var out []clubSession
	for _, ev := range events {
		if ev.StartsAt == nil {
			continue // a ladder has no session date
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(*ev.StartsAt))
		if err != nil || at.After(now) {
			continue // hasn't happened yet
		}
		out = append(out, clubSession{id: ev.ID, at: at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	if window > 0 && len(out) > window {
		out = out[:window]
	}
	return out
}

// ClubRosterCSV is the roster as a spreadsheet.
//
// The first thing a facility asks for, and the reason is not reporting — it is
// that a club's committee lives in email and a spreadsheet is how a volunteer
// who will never install this app still gets to help. Refusing to export is how
// a product becomes the place the data is TRAPPED rather than the place it
// lives, which is the opposite of the system-of-record argument the Club tier
// is sold on.
//
// Same admin gate as the roster view: who is drifting away is management
// information.
func (s *Service) ClubRosterCSV(clubID, callerID string) ([]byte, error) {
	rows, err := s.ClubRoster(clubID, callerID)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("Name,Role,Status,Sessions played,Of last,Missed in a row," +
		"Last played,Dues\n")
	for _, r := range rows {
		role := r.Role
		if role == "" {
			role = "member"
		}
		b.WriteString(strings.Join([]string{
			csvCell(r.FullName),
			csvCell(role),
			csvCell(readableStatus(r.Status)),
			strconv.Itoa(r.Played),
			strconv.Itoa(r.Of),
			strconv.Itoa(r.MissedRun),
			csvCell(dateOnly(r.LastPlayed)),
			csvCell(duesCell(r)),
		}, ","))
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// duesCell writes the dues column the way a treasurer reads it. Blank when the
// club isn't collecting: an empty column says "not applicable" without anyone
// having to be told.
func duesCell(r ClubRosterEntry) string {
	if !r.DuesTracked {
		return ""
	}
	if r.DuesPaid {
		return "Paid"
	}
	return "Unpaid"
}

// readableStatus writes the status the way the screen says it, not the way the
// database stores it. A spreadsheet column reading "never_played" is a leaked
// enum; a committee member reading "Never played" just understands.
func readableStatus(s string) string {
	switch s {
	case ClubStatusActive:
		return "Playing"
	case ClubStatusSlipping:
		return "Slipping away"
	case ClubStatusLapsed:
		return "Away a while"
	case ClubStatusNeverAlong:
		return "Never played"
	default:
		return s
	}
}

// dateOnly trims an RFC3339 timestamp to the date. A spreadsheet cell holding
// "2026-08-17T19:00:00Z" sorts fine and reads like machinery.
func dateOnly(rfc string) string {
	if len(rfc) >= 10 {
		return rfc[:10]
	}
	return rfc
}

// csvCell quotes a value so a name containing a comma — which is most people
// who enter "Surname, First" — cannot shift every column after it.
func csvCell(v string) string {
	v = strings.TrimSpace(v)
	if !strings.ContainsAny(v, ",\"\n\r") {
		return v
	}
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}
