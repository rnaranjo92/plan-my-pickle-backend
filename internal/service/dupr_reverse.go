package service

import (
	"log"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// reverseDuprForMatches durably queues DUPR-side reversals for a SPECIFIC set of
// matches about to be deleted or unmade, then removes their submission rows.
//
// This is the same safety net wipeAllMatches uses, factored out so the paths
// that delete or replace a SUBSET of matches — rebuilding a playoff
// (wipeBracketStage), forfeiting an already-imported game, deleting a played
// grand-final reset game — reverse the official DUPR results instead of
// orphaning them live on players' ratings forever.
//
// Best-effort and non-fatal: a failure here must never block the local mutation
// the caller is performing. Rows land in dupr_pending_deletes (durable, drained
// by the reconciler with backoff); if that table is missing we fall back to an
// inline best-effort delete, matching wipeAllMatches.
func (s *Service) reverseDuprForMatches(eventID string, matchIDs []string) {
	if len(matchIDs) == 0 || s.Dupr == nil {
		return
	}
	defer s.lockDuprEvent(eventID)()
	subs, err := s.sb.Select("dupr_submissions",
		"match_id="+store.In(matchIDs)+"&status=eq.submitted"+
			"&select=match_id,provider_ref,dupr_gen")
	if err != nil || len(subs) == 0 {
		return
	}
	var toDelete [][2]string
	for _, sub := range subs {
		code := asStr(sub, "provider_ref")
		if code == "" {
			continue
		}
		ident := duprIdentifier(asStr(sub, "match_id"), asInt(sub, "dupr_gen"))
		toDelete = append(toDelete, [2]string{code, ident})
	}
	if len(toDelete) == 0 {
		return
	}
	queued := false
	rows := make([]map[string]any, len(toDelete))
	for i, d := range toDelete {
		rows[i] = map[string]any{
			"event_id": eventID, "provider_ref": d[0], "identifier": d[1],
		}
	}
	if _, err := s.sb.Insert("dupr_pending_deletes", rows); err != nil {
		log.Printf("dupr: subset pending-delete queue failed (inline fallback): %v", err)
	} else {
		queued = true
	}
	// Drop the local submission rows for these matches so they aren't re-counted.
	_ = s.sb.Delete("dupr_submissions", "match_id="+store.In(matchIDs))
	if queued {
		go func() {
			if err := s.DrainDuprPendingDeletes(); err != nil {
				log.Printf("dupr: subset reversal drain: %v", err)
			}
		}()
		return
	}
	go func() {
		for _, d := range toDelete {
			var e error
			for attempt := 0; attempt < 3; attempt++ {
				if e = s.Dupr.DeleteMatch(d[0], d[1]); e == nil {
					break
				}
			}
			if e != nil {
				log.Printf("dupr: subset delete %s FAILED: %v (may still be live on DUPR)", d[0], e)
			}
		}
	}()
}
