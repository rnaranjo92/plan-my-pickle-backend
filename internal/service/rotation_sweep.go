package service

import (
	"errors"
	"fmt"
	"log"
	"os"
	"time"
)

// The night must not depend on one phone staying awake.
//
// Auto-advance runs on the ORGANIZER'S device (_maybeAutoAdvance returns unless
// isOwner), there is no wakelock anywhere in the app, and the organizer is
// usually playing too — so their phone goes in a pocket and auto-locks, or the
// browser throttles the backgrounded tab. The buzzer never rings, the round
// never advances, the TV counts to 0:00, and the whole gym stands around
// waiting for someone who is mid-rally on court 3.
//
// Letting participants' devices fire the advance was considered and rejected:
// that grant was deliberately removed, because it let any rostered player —
// including one who had already gone home — rotate the entire room. The server
// is the one actor that is always awake and always allowed.

// rotationSweepGrace is how far past its deadline a round must be before the
// server steps in.
//
// Long enough that the organizer's device is the one that normally advances
// (it fires at 0:00, and its own 45s grace for unreported courts runs inside
// that), short enough that nobody is standing around. This is a backstop, not
// the primary path — if it becomes the thing that advances every round, the
// client-side timer is broken and should be fixed rather than leaned on.
const rotationSweepGrace = 75 * time.Second

// rotationRoundOverdue reports whether a round is late enough for the server to
// take over. Pure, so the rule is testable without a clock or a database.
func rotationRoundOverdue(endsAt, now time.Time, grace time.Duration) bool {
	if endsAt.IsZero() {
		return false // no deadline set — an untimed round is never overdue
	}
	return now.After(endsAt.Add(grace))
}

// SweepOverdueRotationRounds advances any live auto-advance session whose round
// deadline has passed by more than the grace period.
//
// Idempotent and safe to run on every tick: the advance is passed the round it
// believes is current, and AdvanceRotationSession no-ops when that no longer
// matches — so a sweep racing the organizer's device cannot skip a round.
// Paused sessions are excluded by the status filter, and manual-mode sessions
// by the auto_advance filter: an organizer who turned auto-advance off is
// asking to control the pace by hand, and the server must not overrule that.
func (s *Service) SweepOverdueRotationRounds() error {
	if !s.columnReady("rotation_sessions", "auto_advance") {
		return nil // pre-migration: nothing to sweep
	}
	cutoff := time.Now().UTC().Add(-rotationSweepGrace).Format(time.RFC3339)
	rows, err := s.sb.Select("rotation_sessions", rotationSweepFilter(cutoff))
	if err != nil {
		return err
	}
	for _, r := range rows {
		id, round := asStr(r, "id"), asInt(r, "current_round")
		if id == "" || round < 1 {
			continue
		}
		err := s.AdvanceRotationSession(id, round)
		switch {
		case err == nil:
			log.Printf("rotation sweep: advanced %s past round %d "+
				"(organizer's device didn't)", id, round)
		case errors.Is(err, ErrRoundBlocked):
			// Expected and not the sweep's business to resolve: a tied court, or
			// too few players left. The organizer gets the same message on their
			// own screen and is the one who can fix it.
			log.Printf("rotation sweep: %s round %d blocked: %v", id, round, err)
		default:
			log.Printf("rotation sweep: %s round %d failed: %v", id, round, err)
		}
	}
	return nil
}

// rotationSweepFilter builds the query the sweep runs.
//
// Its own function because the filter IS the safety argument: 'live' is what
// excludes a paused night and 'auto_advance is true' is what excludes an
// organizer running the pace by hand. Both exclusions exist here and nowhere
// else, so they get a test rather than a comment.
func rotationSweepFilter(cutoff string) string {
	return "status=eq.live&auto_advance=is.true" +
		"&round_ends_at=lt." + cutoff +
		"&select=id,current_round,round_ends_at&limit=200"
}

// --- overtime: telling the organizer when a round is stuck -------------------

// rotationOvertimeAfter is how far past its deadline a round runs before the
// organizer is told about it.
//
// Deliberately LONGER than rotationSweepGrace. An auto-advance session that is
// working advances at the grace mark and moves its deadline, so it never
// reaches this and nobody is buzzed — the alert only fires when the round is
// genuinely stuck: auto-advance is off and the organizer's device didn't
// advance, or the sweep tried and was blocked by a tied court. Both are cases
// where the night has stopped and only a person can restart it.
//
// This replaced a push at every round START, which told the organizer what was
// already on the screen in their hand.
const rotationOvertimeAfter = rotationSweepGrace + 45*time.Second

// overtimeNotified remembers which (session, round) pairs have been buzzed, so
// a 30-second ticker doesn't repeat the same alert until the round moves.
//
// In memory on purpose: this is a nudge, not a record. A restart at worst
// repeats one push, where a table would mean a migration to run before the fix
// did anything.
type overtimeKey struct {
	sessionID string
	round     int
}

// NotifyOverdueRotationRounds tells organizers about rounds that have run over.
//
// Runs on the same tick as the advance sweep and deliberately changes nothing:
// it never advances, never writes to the session. Paused sessions are excluded
// — a paused night is over-running on purpose.
func (s *Service) NotifyOverdueRotationRounds() error {
	if os.Getenv("ONESIGNAL_REST_API_KEY") == "" {
		return nil // push not configured → don't even query
	}
	cutoff := time.Now().UTC().Add(-rotationOvertimeAfter).Format(time.RFC3339)
	rows, err := s.sb.Select("rotation_sessions",
		"status=eq.live&round_ends_at=lt."+cutoff+
			"&select=id,name,current_round&limit=200")
	if err != nil {
		return err
	}
	for _, r := range rows {
		id, round := asStr(r, "id"), asInt(r, "current_round")
		if id == "" || round < 1 {
			continue
		}
		if !s.claimOvertimeNotice(overtimeKey{id, round}) {
			continue // already told them about this round
		}
		owner, _ := s.OwnerOfRotationSession(id)
		if owner == "" {
			continue
		}
		name := asStr(r, "name")
		if name == "" {
			name = "Rotation session"
		}
		_ = s.sendPush([]string{owner}, name,
			fmt.Sprintf("Round %d has run past its time — the board is waiting.",
				round), "")
	}
	return nil
}

// claimOvertimeNotice reports whether THIS pass should send the alert, marking
// it sent. One buzz per round: the caller runs every 30 seconds.
//
// Also drops the marks for earlier rounds of the same session as it goes, so a
// long-running server doesn't accumulate a key per round of every night played.
func (s *Service) claimOvertimeNotice(k overtimeKey) bool {
	s.overtimeMu.Lock()
	defer s.overtimeMu.Unlock()
	if s.overtimeNotified == nil {
		s.overtimeNotified = map[overtimeKey]bool{}
	}
	if s.overtimeNotified[k] {
		return false
	}
	for prev := range s.overtimeNotified {
		if prev.sessionID == k.sessionID && prev.round < k.round {
			delete(s.overtimeNotified, prev)
		}
	}
	s.overtimeNotified[k] = true
	return true
}

// rotationAbandonedAfter is how long past its last round deadline a session has
// to sit before it counts as walked away from rather than merely paused.
//
// Comfortably longer than any real night, including a long dinner break or an
// organizer who pauses and comes back — but short enough that next week's
// session doesn't inherit last week's board.
const rotationAbandonedAfter = 12 * time.Hour

// rotationSessionAbandoned reports whether a live/paused session's round
// deadline is so far in the past that nobody is coming back to it. Pure.
//
// An unparseable or missing deadline is NOT abandoned: a session that has been
// created but never started has no deadline, and calling that abandoned would
// hide the night the organizer is setting up right now.
func rotationSessionAbandoned(roundEndsAt string, now time.Time) bool {
	ends, err := time.Parse(time.RFC3339, roundEndsAt)
	if err != nil {
		return false
	}
	return now.Sub(ends) > rotationAbandonedAfter
}

// EndAbandonedRotationSessions closes out sessions nobody ever ended.
//
// Challenge ladders got a sweep; rotation never did. So a night the organizer
// walked away from stayed 'live' forever: its standings were never locked, its
// last round was never credited, and it outranked every later session on the
// event's TV link. Ending it is exactly what the organizer would have done, and
// EndRotationSession is used rather than a bare status write so the closing
// round is tallied the same way tapping End would.
func (s *Service) EndAbandonedRotationSessions() error {
	cutoff := time.Now().UTC().Add(-rotationAbandonedAfter).Format(time.RFC3339)
	rows, err := s.sb.Select("rotation_sessions",
		"status=in.(live,paused)"+
			"&round_ends_at=lt."+cutoff+
			"&select=id,round_ends_at&limit=100")
	if err != nil {
		return err
	}
	for _, r := range rows {
		id := asStr(r, "id")
		if id == "" {
			continue
		}
		if err := s.EndRotationSession(id); err != nil {
			log.Printf("rotation sweep: couldn't end abandoned %s: %v", id, err)
			continue
		}
		log.Printf("rotation sweep: ended abandoned session %s "+
			"(last round ended %s)", id, asStr(r, "round_ends_at"))
	}
	return nil
}
