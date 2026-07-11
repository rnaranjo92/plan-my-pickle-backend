package service

import (
	"errors"
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// kudosLabels is the fixed set of always-positive recognitions. Kudos are
// earned, never requested, and never negative — the label set has no downside
// options by design.
var kudosLabels = map[string]bool{
	"Great serve":        true,
	"Nice dinks":         true,
	"Clutch shots":       true,
	"Good sportsmanship": true,
	"Fun to play":        true,
	"Solid partner":      true,
}

// KudosLabels returns the allowed kudos labels (for the client's picker), sorted.
func KudosLabels() []string {
	out := make([]string, 0, len(kudosLabels))
	for l := range kudosLabels {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// GiveKudos records a positive recognition from giverUserID to the ACCOUNT
// behind receiverPlayerID. The receiver must be linked to an account (kudos are
// account-level so they aggregate across every event a person plays); self-kudos
// and unknown labels are rejected; the (giver, receiver, label) unique index
// makes it idempotent — you can recognize a given skill in a player once.
func (s *Service) GiveKudos(giverUserID, receiverPlayerID, label string) error {
	if giverUserID == "" {
		return ErrForbidden
	}
	label = strings.TrimSpace(label)
	if !kudosLabels[label] {
		return errors.New("unknown kudos")
	}
	prow, err := s.sb.SelectOne("players",
		"id=eq."+store.Q(receiverPlayerID)+"&select=user_id")
	if err != nil {
		return err
	}
	if prow == nil {
		return ErrNotFound
	}
	receiverUserID := asStr(prow, "user_id")
	if receiverUserID == "" {
		return errors.New("this player isn't on the app yet — kudos need an account")
	}
	if receiverUserID == giverUserID {
		return errors.New("you can't give yourself kudos")
	}
	_, err = s.sb.Upsert("kudos", "giver_user_id,receiver_user_id,label", map[string]any{
		"giver_user_id":    giverUserID,
		"receiver_user_id": receiverUserID,
		"label":            label,
	})
	return err
}

// kudosForUser tallies the kudos an account has received: per-label counts
// (highest first) and the distinct-giver count (the anti-spam Street Cred
// signal). Best-effort — any error yields an empty tally.
func (s *Service) kudosForUser(userID string) ([]model.KudosTally, int) {
	if userID == "" {
		return []model.KudosTally{}, 0
	}
	rows, err := s.sb.SelectAll("kudos",
		"receiver_user_id=eq."+store.Q(userID)+"&select=label,giver_user_id")
	if err != nil {
		return []model.KudosTally{}, 0
	}
	counts := map[string]int{}
	givers := map[string]bool{}
	for _, r := range rows {
		if l := asStr(r, "label"); l != "" {
			counts[l]++
		}
		if g := asStr(r, "giver_user_id"); g != "" {
			givers[g] = true
		}
	}
	out := make([]model.KudosTally, 0, len(counts))
	for l, c := range counts {
		out = append(out, model.KudosTally{Label: l, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out, len(givers)
}
