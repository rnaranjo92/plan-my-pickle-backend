package service

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Corrections for a rotation night that has already moved on.
//
// Everything here exists because the engine was write-once: a court's winner
// was recorded at the buzzer, wins were tallied by the advance RPC, and from
// then on there was no way back. A wrong "this team won" tap noticed one round
// later was permanent, a mis-tapped End could only be answered by starting a
// brand-new session with zeroed standings, and correcting a past round's score
// fixed the points while leaving the win with the loser. Wrong taps at the
// buzzer, with four people watching and a clock running, are a when.
//
// The data was always sufficient to fix all of it — rotation_round_courts holds
// every round's winner — so the fix is to derive rather than to remember.

// rotationWinsFromCourts counts each player's wins from the stored court
// results. Pure, so the counting rule is testable without a database.
//
// Only 'a'/'b' count. An unreported or undecided court (empty, tied, abandoned)
// credits nobody — the same rule the tally RPC uses, kept identical here so a
// retally can never disagree with a night that was never corrected.
func rotationWinsFromCourts(rows []map[string]any) map[string]int {
	wins := map[string]int{}
	for _, r := range rows {
		var seats [2]string
		switch strings.TrimSpace(asStr(r, "winner")) {
		case "a":
			seats = [2]string{asStr(r, "team_a_p1"), asStr(r, "team_a_p2")}
		case "b":
			seats = [2]string{asStr(r, "team_b_p1"), asStr(r, "team_b_p2")}
		default:
			continue
		}
		for _, id := range seats {
			if id = strings.TrimSpace(id); id != "" {
				wins[id]++
			}
		}
	}
	return wins
}

// retallyRotationWins recomputes every player's win count from the court
// results and writes back only what changed.
//
// GAMES IS DELIBERATELY NOT TOUCHED. Wins are a scoreboard number — the
// standings column and the documented tiebreak — so deriving them is always
// right. Games is an INPUT to the fairness pass that decides who sits out next,
// so rewriting it mid-session would change who plays, which is not what
// somebody fixing a typo asked for.
//
// Idempotent: running it on a night nobody corrected writes nothing.
func (s *Service) retallyRotationWins(sessionID string) error {
	courts, err := s.sb.SelectAll("rotation_round_courts",
		"session_id=eq."+store.Q(sessionID)+
			"&select=winner,team_a_p1,team_a_p2,team_b_p1,team_b_p2")
	if err != nil {
		return fmt.Errorf("couldn't read this session's results: %w", err)
	}
	wins := rotationWinsFromCourts(courts)

	players, err := s.sb.SelectAll("rotation_players",
		"session_id=eq."+store.Q(sessionID)+"&select=id,wins")
	if err != nil {
		return fmt.Errorf("couldn't read this session's players: %w", err)
	}
	for _, p := range players {
		id := asStr(p, "id")
		if id == "" {
			continue
		}
		want := wins[id] // absent = 0, which is the correct answer for a player
		if asInt(p, "wins") == want {
			continue
		}
		if _, uerr := s.sb.Update("rotation_players",
			"id=eq."+store.Q(id),
			map[string]any{"wins": want}); uerr != nil {
			return fmt.Errorf("couldn't correct the win count: %w", uerr)
		}
	}
	return nil
}

// SetRotationCourtWinner is the organizer's correction for a court whose result
// was recorded wrong — including one from a round that has already been played
// out.
//
// Deliberately NOT ReportRotationCourt with a relaxed guard. Reporting is a
// participant action on the live round and must stay pinned to it; this is an
// owner action on the record of the night, and the two want opposite rules.
//
// The MOVEMENT that already happened is not rewound. The wrong pair really did
// play the next round on the next court, and re-seeding a night retroactively
// would invalidate results that were genuinely played. What this fixes is the
// record: the court's winner and everybody's win count. The dialog says so.
func (s *Service) SetRotationCourtWinner(
	sessionID string, round, court int, winner string,
) error {
	winner = strings.TrimSpace(winner)
	if winner != "a" && winner != "b" && winner != "" {
		return errors.New("winner must be 'a', 'b', or empty to clear it")
	}
	if round < 1 || court < 1 {
		return errors.New("round and court are required")
	}
	filter := "session_id=eq." + store.Q(sessionID) +
		"&round=eq." + fmt.Sprint(round) +
		"&court=eq." + fmt.Sprint(court)

	// Confirm the court exists BEFORE writing. A PATCH that matches no rows
	// succeeds and reports nothing, so a typo'd court number used to come back
	// as a cheerful "corrected" having changed nothing at all.
	exists, err := s.sb.SelectOne("rotation_round_courts", filter+"&select=court")
	if err != nil {
		return err
	}
	if exists == nil {
		return fmt.Errorf("%w: there was no court %d in round %d",
			ErrNotFound, court, round)
	}

	patch := map[string]any{"reported_at": nowRFC3339()}
	if winner == "" {
		patch["winner"] = nil
	} else {
		patch["winner"] = winner
	}
	if _, err := s.sb.Update("rotation_round_courts", filter, patch); err != nil {
		return err
	}
	// The whole point of the correction. Without this the board shows one team
	// as the winner while the other team holds the win.
	return s.retallyRotationWins(sessionID)
}

// ReopenRotationSession puts a finished night back on the clock.
//
// End is one control-tap away from Pause and Next on a bar that is visible all
// evening, and the stop rule can also end the night on its own ("there isn't
// time for another full round"). When the venue then extends the booking, or
// the tap was simply wrong, the only previous answer was a brand-new session —
// which means zeroed standings and a roster typed in again, i.e. losing the
// evening to fix a mis-tap.
//
// Nothing is recomputed: every round that was played keeps its results and
// every win stays credited. The session resumes on the round it ended on with a
// fresh timer, exactly as a long pause would have left it.
func (s *Service) ReopenRotationSession(sessionID string) error {
	srow, err := s.sb.SelectOne("rotation_sessions",
		"id=eq."+store.Q(sessionID)+"&select=status,round_minutes")
	if err != nil {
		return err
	}
	if srow == nil {
		return ErrNotFound
	}
	if st := asStr(srow, "status"); st != "done" {
		// Not an error worth a scary message — the session is already open.
		return fmt.Errorf("this session is already %s", st)
	}
	mins := asInt(srow, "round_minutes")
	if mins <= 0 {
		mins = 12
	}
	// Compare-and-set on 'done' so two organizers tapping Reopen at once can't
	// have the second one restart a round the first already began.
	rows, err := s.sb.Update("rotation_sessions",
		"id=eq."+store.Q(sessionID)+"&status=eq.done",
		map[string]any{
			"status":        "live",
			"round_ends_at": roundEndsAt(mins),
			"paused_at":     nil,
		})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("this session changed while you were reopening it — " +
			"check the board before trying again")
	}
	return nil
}
