package service

import (
	"errors"
	"fmt"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// validateGame enforces one pickleball game: the winner must reach points_to_win
// AND win by at least win_by, with the deuce cap — past the target a game ends on
// exactly a win_by lead out of deuce (12–10 ok, 15–2 impossible). With no win-by
// margin it ends at the target. Identical to the Flutter client's
// validatePickleballScore.
func validateGame(t1, t2, ptw, winBy int) error {
	if t1 < 0 || t2 < 0 {
		return errors.New("scores must be non-negative")
	}
	if t1 == t2 {
		return errors.New("a pickleball game cannot end in a tie")
	}
	hi, lo := t1, t2
	if t2 > t1 {
		hi, lo = t2, t1
	}
	if hi < ptw {
		return fmt.Errorf("winning score must reach %d", ptw)
	}
	if hi-lo < winBy {
		return fmt.Errorf("must win by %d (got %d–%d)", winBy, hi, lo)
	}
	if winBy >= 2 {
		if hi > ptw && (hi-lo != winBy || lo < ptw-1) {
			return fmt.Errorf("past %d a game ends on a %d-point lead, e.g. %d–%d", ptw, winBy, ptw+winBy-1, ptw-1)
		}
	} else if hi > ptw {
		return fmt.Errorf("a game to %d with no win-by margin ends at %d", ptw, ptw)
	}
	return nil
}

// validateSeries checks that games form a complete, legal best-of-`bestOf` result
// (each game legal; a team won the required majority; the series stopped the
// moment it was clinched) and returns the series winner (1|2) plus each side's
// total points across all games. bestOf is 1 or 3 (any odd N works — a side must
// win bestOf/2+1 games). The winner is decided by GAMES won, not total points.
func validateSeries(games []model.GameScore, bestOf, ptw, winBy int) (winner, t1total, t2total int, err error) {
	if bestOf < 1 {
		bestOf = 1
	}
	if len(games) == 0 {
		return 0, 0, 0, errors.New("no game scores provided")
	}
	if len(games) > bestOf {
		return 0, 0, 0, fmt.Errorf("a best-of-%d match has at most %d games", bestOf, bestOf)
	}
	needed := bestOf/2 + 1
	w1, w2 := 0, 0
	for i, g := range games {
		if e := validateGame(g.Team1, g.Team2, ptw, winBy); e != nil {
			return 0, 0, 0, fmt.Errorf("game %d: %w", i+1, e)
		}
		t1total += g.Team1
		t2total += g.Team2
		if g.Team1 > g.Team2 {
			w1++
		} else {
			w2++
		}
		// The series must stop the instant a side clinches — no dead games.
		if (w1 >= needed || w2 >= needed) && i != len(games)-1 {
			return 0, 0, 0, errors.New("the series is already decided — remove the extra game(s)")
		}
	}
	if w1 < needed && w2 < needed {
		return 0, 0, 0, fmt.Errorf("best-of-%d isn't decided — a team needs to win %d games (it's %d–%d)", bestOf, needed, w1, w2)
	}
	if w1 > w2 {
		return 1, t1total, t2total, nil
	}
	return 2, t1total, t2total, nil
}
