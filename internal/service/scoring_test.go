package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

func gm(a, b int) model.GameScore { return model.GameScore{Team1: a, Team2: b} }

func TestValidateSeries(t *testing.T) {
	cases := []struct {
		name               string
		games              []model.GameScore
		bestOf, ptw, winBy int
		wantWinner         int // 0 = expect an error
		wantT1, wantT2     int
	}{
		// Single game (best-of-1).
		{"single t1", []model.GameScore{gm(11, 9)}, 1, 11, 2, 1, 11, 9},
		{"single t2", []model.GameScore{gm(9, 11)}, 1, 11, 2, 2, 9, 11},
		{"deuce ok 12-10", []model.GameScore{gm(12, 10)}, 1, 11, 2, 1, 12, 10},
		{"win-by-1 to 15", []model.GameScore{gm(15, 14)}, 1, 15, 1, 1, 15, 14},

		// Best-of-3 — totals are the SUM across games; winner is games won.
		{"bo3 sweep t1", []model.GameScore{gm(11, 9), gm(11, 5)}, 3, 11, 2, 1, 22, 14},
		{"bo3 sweep t2", []model.GameScore{gm(8, 11), gm(6, 11)}, 3, 11, 2, 2, 14, 22},
		{"bo3 three-game t2", []model.GameScore{gm(11, 9), gm(5, 11), gm(9, 11)}, 3, 11, 2, 2, 25, 31},
		// t1 wins the series (games 2,3) despite scoring FEWER total points.
		{"bo3 comeback t1 fewer points", []model.GameScore{gm(5, 11), gm(11, 9), gm(11, 9)}, 3, 11, 2, 1, 27, 29},

		// Errors.
		{"single game on bo3 (incomplete)", []model.GameScore{gm(11, 9)}, 3, 11, 2, 0, 0, 0},
		{"bo3 split 1-1 incomplete", []model.GameScore{gm(11, 9), gm(9, 11)}, 3, 11, 2, 0, 0, 0},
		{"bo3 extra game after sweep", []model.GameScore{gm(11, 0), gm(11, 0), gm(11, 0)}, 3, 11, 2, 0, 0, 0},
		{"too many games", []model.GameScore{gm(11, 0), gm(11, 0), gm(11, 0), gm(11, 0)}, 3, 11, 2, 0, 0, 0},
		{"no games", nil, 1, 11, 2, 0, 0, 0},
		{"illegal game tie", []model.GameScore{gm(11, 11)}, 1, 11, 2, 0, 0, 0},
		{"illegal game no win-by-2", []model.GameScore{gm(11, 10)}, 1, 11, 2, 0, 0, 0},
		{"illegal game deuce 15-2", []model.GameScore{gm(15, 2)}, 1, 11, 2, 0, 0, 0},
		{"illegal game below target", []model.GameScore{gm(9, 7)}, 1, 11, 2, 0, 0, 0},
		{"bo3 second game illegal", []model.GameScore{gm(11, 9), gm(11, 10)}, 3, 11, 2, 0, 0, 0},
	}
	for _, c := range cases {
		w, t1, t2, err := validateSeries(c.games, c.bestOf, c.ptw, c.winBy)
		if c.wantWinner == 0 {
			if err == nil {
				t.Errorf("%s: expected an error, got winner=%d (%d–%d)", c.name, w, t1, t2)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if w != c.wantWinner || t1 != c.wantT1 || t2 != c.wantT2 {
			t.Errorf("%s: got winner=%d totals=%d–%d, want winner=%d totals=%d–%d",
				c.name, w, t1, t2, c.wantWinner, c.wantT1, c.wantT2)
		}
	}
}
