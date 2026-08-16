package service

import (
	"sort"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// A player's recent form, for the chart on their own card.
//
// Derived, not stored. Nothing in the schema keeps a history — games_played is
// a running counter and dupr_rating is a single current value — so the only
// real time series available is the one implied by results: each event has a
// date, and each event has a box score.
//
// POINT DIFFERENTIAL is the value plotted rather than wins. A win count says
// how often; a differential says whether you were close, and a run of narrow
// losses looks completely different from a run of hidings. It's also signed,
// which gives the chart a zero line to read against instead of a bar length to
// compare.

// maxFormEvents caps the walk. Each event costs a standings computation, so
// this is deliberately small — it's a sparkline on a profile, not a report.
const maxFormEvents = 10

// MyForm returns the caller's last events, oldest first so a chart can plot
// them left to right.
//
// Best-effort per event: one event whose standings fail to compute drops itself
// rather than emptying the chart.
func (s *Service) MyForm(userID, email string) ([]model.PlayerFormEntry, error) {
	if strings.TrimSpace(userID) == "" {
		return []model.PlayerFormEntry{}, nil
	}
	// The caller's player rows — a person can have several (one per event), so
	// the match is on the SET, not a single id.
	mine := map[string]bool{}
	if rows, err := s.sb.Select("players",
		"user_id=eq."+store.Q(userID)+"&select=id"); err == nil {
		for _, r := range rows {
			if id := asStr(r, "id"); id != "" {
				mine[id] = true
			}
		}
	}
	if len(mine) == 0 {
		return []model.PlayerFormEntry{}, nil
	}

	evs, err := s.MyEvents(userID, email)
	if err != nil {
		return nil, err
	}
	// Only events that have actually happened: a chart of the future is noise,
	// and an undated draft has nowhere to sit on a time axis.
	now := time.Now().UTC()
	type dated struct {
		ev model.Event
		at time.Time
	}
	played := make([]dated, 0, len(evs))
	for _, e := range evs {
		if e.StartsAt == nil {
			continue
		}
		t, perr := time.Parse(time.RFC3339, *e.StartsAt)
		if perr != nil || t.After(now) {
			continue
		}
		played = append(played, dated{ev: e, at: t})
	}
	// Newest first, so the cap keeps the RECENT ones, then reversed for the
	// chart. Taking the first N of an unsorted list would show a random decade.
	sort.SliceStable(played, func(i, j int) bool {
		return played[i].at.After(played[j].at)
	})
	if len(played) > maxFormEvents {
		played = played[:maxFormEvents]
	}

	out := make([]model.PlayerFormEntry, 0, len(played))
	for _, d := range played {
		rows, rerr := s.standingRowsSince(d.ev.ID, "", s.seasonStartFor(d.ev.ID))
		if rerr != nil {
			continue
		}
		var mineRow *model.Standing
		for i := range rows {
			if mine[rows[i].PlayerID] {
				mineRow = &rows[i]
				break
			}
		}
		// No row means the player registered but never played a scored game.
		// Plotting a zero there would read as "drew every match".
		if mineRow == nil || mineRow.GamesPlayed == 0 {
			continue
		}
		out = append(out, model.PlayerFormEntry{
			EventID:     d.ev.ID,
			EventName:   d.ev.Name,
			PlayedAt:    d.at.Format(time.RFC3339),
			GamesPlayed: mineRow.GamesPlayed,
			Wins:        mineRow.Wins,
			Losses:      mineRow.Losses,
			PointDiff:   mineRow.PointDiff,
		})
	}
	// Oldest first for the chart.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
