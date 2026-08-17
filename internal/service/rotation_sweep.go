package service

import (
	"errors"
	"log"
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
