package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Ladder phase 2 — player-driven challenges + deadline timers. Players challenge
// someone above them (within range); the challenged player accepts/declines; the
// match is played + reported, or the reconciler's timers fire. Every
// position-changing transition goes through the resolve_ladder_challenge RPC so
// the status flip and the reorder are ONE atomic transaction (0069).

// ErrChallengeConflict is returned when a challenge was already resolved by
// another actor or a reconciler tick (the atomic claim lost the race).
var ErrChallengeConflict = errors.New("this challenge was already resolved")

const (
	// challengeCooldown blocks re-challenging the same pair right after a void
	// (prevents the accept-stall-recycle ducking loop).
	challengeCooldown = 24 * time.Hour
	// Caps on a single entrant's simultaneously-active challenges (blunts farming).
	maxActiveOutgoing = 3
	maxActiveIncoming = 5
	// Reminder fires when a deadline is within this window.
	challengeReminderWindow = 24 * time.Hour
)

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// challengeOn reports whether the challenges table exists (migration applied).
func (s *Service) challengeOn() bool {
	return s.columnReady("ladder_challenges", "status")
}

// MyLadderEntrant returns the caller's entrant id on a division (or ""), so the
// client can show challenge affordances only on rungs above the viewer.
func (s *Service) MyLadderEntrant(userID, div string) string {
	return s.callerEntrantID(userID, div)
}

// callerEntrantID returns the caller's entrant id on a division (their linked
// account's entrant), or "" if they aren't a linked entrant there.
func (s *Service) callerEntrantID(userID, div string) string {
	pid := s.playerIDForUser(userID)
	if pid == "" {
		return ""
	}
	row, err := s.sb.SelectOne("ladder_entrants",
		"league_bracket_id=eq."+store.Q(div)+"&player_id=eq."+store.Q(pid)+"&select=id&limit=1")
	if err != nil || row == nil {
		return ""
	}
	return asStr(row, "id")
}

// entrantUserID resolves an entrant → its linked account user id (or "").
func (s *Service) entrantUserID(entrantID string) string {
	e, err := s.sb.SelectOne("ladder_entrants", "id=eq."+store.Q(entrantID)+"&select=player_id")
	if err != nil || e == nil {
		return ""
	}
	pid := asStr(e, "player_id")
	if pid == "" {
		return ""
	}
	p, err := s.sb.SelectOne("players", "id=eq."+store.Q(pid)+"&select=user_id")
	if err != nil || p == nil {
		return ""
	}
	return asStr(p, "user_id")
}

// leagueIDOfDivision returns the league id a division belongs to (or "").
func (s *Service) leagueIDOfDivision(div string) string {
	row, err := s.sb.SelectOne("league_brackets", "id=eq."+store.Q(div)+"&select=league_id")
	if err != nil || row == nil {
		return ""
	}
	return asStr(row, "league_id")
}

// LadderOwnerOfChallenge resolves a challenge → its division → league → owner, so
// the HTTP layer / overrides can bind on the challenge id alone (no confused
// deputy via a separate division path param).
func (s *Service) LadderOwnerOfChallenge(challengeID string) (string, error) {
	ch, err := s.sb.SelectOne("ladder_challenges",
		"id=eq."+store.Q(challengeID)+"&select=league_bracket_id")
	if err != nil {
		return "", err
	}
	if ch == nil {
		return "", ErrNotFound
	}
	return s.LadderOwner(asStr(ch, "league_bracket_id"))
}

// loadChallenge reads a challenge row (raw map) by id.
func (s *Service) loadChallenge(challengeID string) (map[string]any, error) {
	ch, err := s.sb.SelectOne("ladder_challenges", "id=eq."+store.Q(challengeID)+"&select=*")
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, ErrNotFound
	}
	return ch, nil
}

// isLeagueOwner reports whether userID owns the league behind a division.
func (s *Service) isLeagueOwner(userID, div string) bool {
	if userID == "" {
		return false
	}
	owner, err := s.LadderOwner(div)
	return err == nil && owner != "" && owner == userID
}

// ladderConfigForDivision reads the reorder model + challenge range for a division.
func (s *Service) ladderRangeForDivision(div string) int {
	if !s.columnReady("leagues", "ladder_challenge_range") {
		return 0
	}
	lid := s.leagueIDOfDivision(div)
	if lid == "" {
		return 0
	}
	lg, err := s.sb.SelectOne("leagues", "id=eq."+store.Q(lid)+"&select=ladder_challenge_range,ladder_response_days,ladder_play_days")
	if err != nil || lg == nil {
		return 0
	}
	return asInt(lg, "ladder_challenge_range")
}

func (s *Service) ladderWindowDays(div string) (respondDays, playDays int) {
	respondDays, playDays = 7, 14
	lid := s.leagueIDOfDivision(div)
	if lid == "" {
		return
	}
	lg, err := s.sb.SelectOne("leagues", "id=eq."+store.Q(lid)+"&select=ladder_response_days,ladder_play_days")
	if err != nil || lg == nil {
		return
	}
	if r := asInt(lg, "ladder_response_days"); r > 0 {
		respondDays = r
	}
	if p := asInt(lg, "ladder_play_days"); p > 0 {
		playDays = p
	}
	return
}

// IssueChallenge creates a pending challenge from the caller's entrant to an
// entrant ABOVE them on the ladder (within the configured range).
func (s *Service) IssueChallenge(userID, div string, req model.IssueChallengeRequest) (model.LadderChallenge, error) {
	if !s.challengeOn() {
		return model.LadderChallenge{}, errors.New("challenges are not available yet")
	}
	challenged := strings.TrimSpace(req.ChallengedEntrantID)
	if challenged == "" {
		return model.LadderChallenge{}, errors.New("challengedEntrantId is required")
	}
	challenger := s.callerEntrantID(userID, div)
	if challenger == "" {
		return model.LadderChallenge{}, errors.New("you're not on this ladder — the organizer must add you first")
	}
	if challenger == challenged {
		return model.LadderChallenge{}, errors.New("you can't challenge yourself")
	}
	// Both must be on THIS division; the challenged must be account-linked (an
	// account-less entrant can never respond → would be auto-forfeited for free).
	ce, err := s.sb.SelectOne("ladder_entrants",
		"id=eq."+store.Q(challenged)+"&select=league_bracket_id,position,player_id")
	if err != nil {
		return model.LadderChallenge{}, err
	}
	if ce == nil || asStr(ce, "league_bracket_id") != div {
		return model.LadderChallenge{}, errors.New("that player isn't on this ladder")
	}
	if asStr(ce, "player_id") == "" {
		return model.LadderChallenge{}, errors.New("that player has no linked account yet — ask the organizer to record your match")
	}
	me, err := s.sb.SelectOne("ladder_entrants", "id=eq."+store.Q(challenger)+"&select=position")
	if err != nil || me == nil {
		return model.LadderChallenge{}, errors.New("could not read your ladder position")
	}
	myPos, theirPos := asInt(me, "position"), asInt(ce, "position")
	// You may only challenge UP (a higher rank = smaller position).
	if theirPos >= myPos {
		return model.LadderChallenge{}, errors.New("you can only challenge players ranked above you")
	}
	if rng := s.ladderRangeForDivision(div); rng > 0 && (myPos-theirPos) > rng {
		return model.LadderChallenge{}, fmt.Errorf("you can only challenge up to %d spots above you", rng)
	}
	// No duplicate active challenge (partial-unique backstops the race too).
	if dup, _ := s.sb.SelectOne("ladder_challenges",
		"challenger_entrant_id=eq."+store.Q(challenger)+"&challenged_entrant_id=eq."+store.Q(challenged)+
			"&status=in.(pending,accepted)&select=id&limit=1"); dup != nil {
		return model.LadderChallenge{}, errors.New("you already have an active challenge against this player")
	}
	// Cooldown: don't let a just-voided pair be immediately re-challenged.
	since := rfc3339(time.Now().Add(-challengeCooldown))
	// Only voids that came from an accepted-then-unplayed challenge (play_by set)
	// trip the cooldown — that's the accept-stall-recycle ducking loop. Voids from
	// an external reorder (organizer result / manual move; play_by null) must NOT
	// block a legitimate re-challenge.
	if recent, _ := s.sb.SelectOne("ladder_challenges",
		"challenger_entrant_id=eq."+store.Q(challenger)+"&challenged_entrant_id=eq."+store.Q(challenged)+
			"&status=eq.voided&play_by=not.is.null&resolved_at=gt."+store.Q(since)+"&select=id&limit=1"); recent != nil {
		return model.LadderChallenge{}, errors.New("please wait a day before re-challenging this player")
	}
	// Caps on active challenges.
	if s.activeChallengeCount("challenger_entrant_id", challenger) >= maxActiveOutgoing {
		return model.LadderChallenge{}, errors.New("you have too many open challenges — resolve one first")
	}
	if s.activeChallengeCount("challenged_entrant_id", challenged) >= maxActiveIncoming {
		return model.LadderChallenge{}, errors.New("that player has too many pending challenges right now")
	}

	respondDays, _ := s.ladderWindowDays(div)
	respondBy := rfc3339(time.Now().Add(time.Duration(respondDays) * 24 * time.Hour))
	rows, err := s.sb.Insert("ladder_challenges", map[string]any{
		"league_bracket_id":     div,
		"challenger_entrant_id": challenger,
		"challenged_entrant_id": challenged,
		"status":                "pending",
		"respond_by":            respondBy,
	})
	if err != nil {
		return model.LadderChallenge{}, err
	}
	if len(rows) == 0 {
		return model.LadderChallenge{}, errors.New("challenge insert returned no row")
	}
	out := s.hydrateChallenge(rows[0])
	// Notify the challenged player (push + bell).
	s.notifyChallenge(challenged, "challenge",
		fmt.Sprintf("%s challenged you on the ladder — respond by %s",
			out.ChallengerName, humanDay(respondBy)), div)
	return out, nil
}

func (s *Service) activeChallengeCount(col, entrantID string) int {
	rows, err := s.sb.Select("ladder_challenges",
		col+"=eq."+store.Q(entrantID)+"&status=in.(pending,accepted)&select=id")
	if err != nil {
		return 0
	}
	return len(rows)
}

// AcceptChallenge marks a pending challenge accepted and starts the play timer.
// Challenged-party (or owner) only.
func (s *Service) AcceptChallenge(userID, challengeID string) error {
	ch, party, err := s.authorizeChallenge(userID, challengeID)
	if err != nil {
		return err
	}
	if party != "challenged" && party != "owner" {
		return ErrForbidden
	}
	_, playDays := s.ladderWindowDays(asStr(ch, "league_bracket_id"))
	playBy := rfc3339(time.Now().Add(time.Duration(playDays) * 24 * time.Hour))
	updated, err := s.sb.Update("ladder_challenges",
		"id=eq."+store.Q(challengeID)+"&status=eq.pending",
		map[string]any{"status": "accepted", "play_by": playBy})
	if err != nil {
		return err
	}
	if len(updated) == 0 {
		return ErrChallengeConflict
	}
	s.notifyChallenge(asStr(ch, "challenger_entrant_id"), "challenge",
		fmt.Sprintf("%s accepted your challenge — play by %s",
			s.entrantName(asStr(ch, "challenged_entrant_id")), humanDay(playBy)),
		asStr(ch, "league_bracket_id"))
	return nil
}

// CancelChallenge withdraws a still-PENDING challenge (challenger or owner only;
// cancelling after accept is forbidden so it can't be used to duck a loss).
func (s *Service) CancelChallenge(userID, challengeID string) error {
	ch, party, err := s.authorizeChallenge(userID, challengeID)
	if err != nil {
		return err
	}
	if party != "challenger" && party != "owner" {
		return ErrForbidden
	}
	updated, err := s.sb.Update("ladder_challenges",
		"id=eq."+store.Q(challengeID)+"&status=eq.pending",
		map[string]any{"status": "cancelled", "resolved_at": now()})
	if err != nil {
		return err
	}
	if len(updated) == 0 {
		return ErrChallengeConflict
	}
	s.notifyChallenge(asStr(ch, "challenged_entrant_id"), "challenge",
		fmt.Sprintf("%s cancelled their challenge", s.entrantName(asStr(ch, "challenger_entrant_id"))),
		asStr(ch, "league_bracket_id"))
	return nil
}

// DeclineChallenge concedes (the challenger wins by concession). Challenged (or
// owner) only. Atomic via resolve_ladder_challenge (claim + reorder in one tx).
func (s *Service) DeclineChallenge(userID, challengeID string) error {
	ch, party, err := s.authorizeChallenge(userID, challengeID)
	if err != nil {
		return err
	}
	if party != "challenged" && party != "owner" {
		return ErrForbidden
	}
	div := asStr(ch, "league_bracket_id")
	final, err := s.resolveChallenge(challengeID, []string{"pending"}, "declined",
		"challenger", true, "", true, s.ladderRangeForDivision(div))
	if err != nil {
		return err
	}
	if final == "voided" {
		s.notifyBoth(ch, "The ladder changed — that challenge was voided (no position change).")
	} else {
		s.notifyBoth(ch, fmt.Sprintf("%s declined — %s moves up",
			s.entrantName(asStr(ch, "challenged_entrant_id")),
			s.entrantName(asStr(ch, "challenger_entrant_id"))))
	}
	return nil
}

// ReportChallenge records a played result. WinnerSide is challenger|challenged|
// tie — mapped server-side against the challenge row. Either party or owner.
func (s *Service) ReportChallenge(userID, challengeID string, req model.ReportChallengeRequest) error {
	ch, party, err := s.authorizeChallenge(userID, challengeID)
	if err != nil {
		return err
	}
	if party == "" {
		return ErrForbidden
	}
	side := strings.TrimSpace(strings.ToLower(req.WinnerSide))
	if side != "challenger" && side != "challenged" && side != "tie" {
		return errors.New("winnerSide must be challenger, challenged, or tie")
	}
	// Report can claim from pending/accepted, and can even reverse a stale void
	// (a real result should beat a play_by timeout).
	if _, err := s.resolveChallenge(challengeID, []string{"pending", "accepted", "voided"},
		"completed", side, true, strings.TrimSpace(req.Score), false, 0); err != nil {
		return err
	}
	s.notifyBoth(ch, "Your ladder challenge result was recorded")
	return nil
}

// resolveChallenge invokes the atomic resolver RPC and returns the challenge's
// FINAL status (which may differ from newStatus — range re-validation can void a
// forfeit/decline). Returns ErrChallengeConflict when the row was already
// resolved (the RPC returns {claimed:false} rather than raising, since PostgREST
// strips RAISE messages).
func (s *Service) resolveChallenge(challengeID string, expected []string, newStatus, winnerSide string, recordMatch bool, score string, revalidate bool, rng int) (string, error) {
	payload := map[string]any{
		"p_challenge":    challengeID,
		"p_expected":     expected,
		"p_new_status":   newStatus,
		"p_record_match": recordMatch,
		"p_revalidate":   revalidate,
		"p_range":        rng,
	}
	if winnerSide != "" {
		payload["p_winner_side"] = winnerSide
	}
	if score != "" {
		payload["p_score"] = score
	}
	body, err := s.sb.RPC("resolve_ladder_challenge", payload)
	if err != nil {
		return "", err
	}
	var res struct {
		Claimed bool   `json:"claimed"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", err
	}
	if !res.Claimed {
		return "", ErrChallengeConflict
	}
	return res.Status, nil
}

// authorizeChallenge loads a challenge and returns the caller's role:
// "challenger", "challenged", "owner", or "" (no rights).
func (s *Service) authorizeChallenge(userID, challengeID string) (map[string]any, string, error) {
	ch, err := s.loadChallenge(challengeID)
	if err != nil {
		return nil, "", err
	}
	div := asStr(ch, "league_bracket_id")
	mine := s.callerEntrantID(userID, div)
	switch {
	case mine != "" && mine == asStr(ch, "challenger_entrant_id"):
		return ch, "challenger", nil
	case mine != "" && mine == asStr(ch, "challenged_entrant_id"):
		return ch, "challenged", nil
	case s.isLeagueOwner(userID, div):
		return ch, "owner", nil
	}
	return ch, "", nil
}

func (s *Service) entrantName(entrantID string) string {
	row, err := s.sb.SelectOne("ladder_entrants", "id=eq."+store.Q(entrantID)+"&select=display_name")
	if err != nil || row == nil {
		return "A player"
	}
	if n := asStr(row, "display_name"); n != "" {
		return n
	}
	return "A player"
}

// hydrateChallenge maps a challenge row + resolves both display names.
func (s *Service) hydrateChallenge(m map[string]any) model.LadderChallenge {
	c := model.LadderChallenge{
		ID:                  asStr(m, "id"),
		LeagueBracketID:     asStr(m, "league_bracket_id"),
		ChallengerEntrantID: asStr(m, "challenger_entrant_id"),
		ChallengedEntrantID: asStr(m, "challenged_entrant_id"),
		Status:              asStr(m, "status"),
		RespondBy:           asStr(m, "respond_by"),
		PlayBy:              asStr(m, "play_by"),
		ResultMatchID:       asStrPtr(m, "result_match_id"),
		CreatedAt:           asStr(m, "created_at"),
		ResolvedAt:          asStr(m, "resolved_at"),
	}
	c.ChallengerName = s.entrantName(c.ChallengerEntrantID)
	c.ChallengedName = s.entrantName(c.ChallengedEntrantID)
	return c
}

// ListChallenges returns a division's active + recently-resolved challenges.
func (s *Service) ListChallenges(div string) ([]model.LadderChallenge, error) {
	if !s.challengeOn() {
		return []model.LadderChallenge{}, nil
	}
	rows, err := s.sb.Select("ladder_challenges",
		"league_bracket_id=eq."+store.Q(div)+"&order=created_at.desc&limit=100&select=*")
	if err != nil {
		return nil, err
	}
	out := make([]model.LadderChallenge, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.hydrateChallenge(r))
	}
	return out, nil
}

// MyChallenges returns the challenges the caller is a party to (across ladders),
// newest first, flagged from the caller's perspective.
func (s *Service) MyChallenges(userID string) ([]model.LadderChallenge, error) {
	if !s.challengeOn() {
		return []model.LadderChallenge{}, nil
	}
	pid := s.playerIDForUser(userID)
	if pid == "" {
		return []model.LadderChallenge{}, nil
	}
	ents, err := s.sb.SelectAll("ladder_entrants", "player_id=eq."+store.Q(pid)+"&select=id")
	if err != nil || len(ents) == 0 {
		return []model.LadderChallenge{}, nil
	}
	ids := make([]string, 0, len(ents))
	for _, e := range ents {
		ids = append(ids, asStr(e, "id"))
	}
	inList := store.In(ids)
	rows, err := s.sb.Select("ladder_challenges",
		"or=(challenger_entrant_id."+inList+",challenged_entrant_id."+inList+")"+
			"&order=created_at.desc&limit=100&select=*")
	if err != nil {
		return nil, err
	}
	mineSet := map[string]bool{}
	for _, id := range ids {
		mineSet[id] = true
	}
	out := make([]model.LadderChallenge, 0, len(rows))
	for _, r := range rows {
		c := s.hydrateChallenge(r)
		c.Mine = true
		c.IsChallenger = mineSet[c.ChallengerEntrantID]
		out = append(out, c)
	}
	return out, nil
}

// notifyChallenge pushes + files a bell row to an entrant's linked account.
func (s *Service) notifyChallenge(entrantID, typ, body, div string) {
	uid := s.entrantUserID(entrantID)
	if uid == "" {
		return
	}
	link := "feed"
	if lid := s.leagueIDOfDivision(div); lid != "" {
		link = "league:" + lid
	}
	s.notifyUser(uid, typ, "", "", body, link)
}

// notifyBoth notifies both parties of a challenge (best-effort).
func (s *Service) notifyBoth(ch map[string]any, body string) {
	div := asStr(ch, "league_bracket_id")
	s.notifyChallenge(asStr(ch, "challenger_entrant_id"), "challenge", body, div)
	s.notifyChallenge(asStr(ch, "challenged_entrant_id"), "challenge", body, div)
}

// SweepLadderChallenges is the timer engine (reconciler tick): auto-forfeit
// pending challenges past respond_by, void accepted ones past play_by, and send
// 24h deadline reminders. Best-effort; each row is claimed atomically so two
// ticks (or instances) can't double-process.
func (s *Service) SweepLadderChallenges() {
	if !s.challengeOn() {
		return
	}
	nowStr := rfc3339(time.Now())
	soonStr := rfc3339(time.Now().Add(challengeReminderWindow))

	// 1. respond_by passed → forfeit to challenger (atomic; re-validates range).
	if pend, err := s.sb.Select("ladder_challenges",
		"status=eq.pending&respond_by=lt."+store.Q(nowStr)+"&select=*&limit=200"); err == nil {
		for _, ch := range pend {
			id := asStr(ch, "id")
			div := asStr(ch, "league_bracket_id")
			final, err := s.resolveChallenge(id, []string{"pending"}, "forfeited",
				"challenger", true, "", true, s.ladderRangeForDivision(div))
			if err != nil {
				if !errors.Is(err, ErrChallengeConflict) {
					log.Printf("sweep: forfeit challenge %s: %v", id, err)
				}
				continue
			}
			// Range re-validation may have voided it instead of forfeiting →
			// notify what actually happened.
			if final == "voided" {
				s.notifyBoth(ch, "A ladder challenge expired but the ladder had changed — voided, no position change.")
			} else {
				s.notifyBoth(ch, fmt.Sprintf("%s didn't respond in time — %s moves up",
					s.entrantName(asStr(ch, "challenged_entrant_id")),
					s.entrantName(asStr(ch, "challenger_entrant_id"))))
			}
		}
	}

	// 2. play_by passed → void (no auto-award for an unplayed match).
	if acc, err := s.sb.Select("ladder_challenges",
		"status=eq.accepted&play_by=lt."+store.Q(nowStr)+"&select=*&limit=200"); err == nil {
		for _, ch := range acc {
			id := asStr(ch, "id")
			updated, uerr := s.sb.Update("ladder_challenges",
				"id=eq."+store.Q(id)+"&status=eq.accepted",
				map[string]any{"status": "voided", "resolved_at": now()})
			if uerr != nil || len(updated) == 0 {
				continue
			}
			s.notifyBoth(ch, "Your ladder challenge expired unplayed — no position change. Re-challenge when you're both free.")
		}
	}

	// 3. Reminders: respond_by within 24h (pending, not yet reminded).
	if rem, err := s.sb.Select("ladder_challenges",
		"status=eq.pending&reminded_respond=eq.false&respond_by=gt."+store.Q(nowStr)+
			"&respond_by=lt."+store.Q(soonStr)+"&select=*&limit=200"); err == nil {
		for _, ch := range rem {
			id := asStr(ch, "id")
			claimed, uerr := s.sb.Update("ladder_challenges",
				"id=eq."+store.Q(id)+"&status=eq.pending&reminded_respond=eq.false",
				map[string]any{"reminded_respond": true})
			if uerr != nil || len(claimed) == 0 {
				continue
			}
			s.notifyChallenge(asStr(ch, "challenged_entrant_id"), "challenge",
				fmt.Sprintf("Reminder: respond to %s's ladder challenge by %s",
					s.entrantName(asStr(ch, "challenger_entrant_id")), humanDay(asStr(ch, "respond_by"))),
				asStr(ch, "league_bracket_id"))
		}
	}

	// 4. Reminders: play_by within 24h (accepted, not yet reminded).
	if rem, err := s.sb.Select("ladder_challenges",
		"status=eq.accepted&reminded_play=eq.false&play_by=gt."+store.Q(nowStr)+
			"&play_by=lt."+store.Q(soonStr)+"&select=*&limit=200"); err == nil {
		for _, ch := range rem {
			id := asStr(ch, "id")
			claimed, uerr := s.sb.Update("ladder_challenges",
				"id=eq."+store.Q(id)+"&status=eq.accepted&reminded_play=eq.false",
				map[string]any{"reminded_play": true})
			if uerr != nil || len(claimed) == 0 {
				continue
			}
			s.notifyBoth(ch, fmt.Sprintf("Reminder: play your ladder challenge by %s",
				humanDay(asStr(ch, "play_by"))))
		}
	}
}

// voidActiveChallengesForEntrant voids any pending/accepted challenge the entrant
// is a party to and notifies the OTHER side. Called when the entrant's ladder
// position changes OUTSIDE its own challenge resolution (manual reorder, an
// organizer-recorded result, or removal) — a pending challenge against stale
// relative positions must not silently resolve later. Best-effort.
func (s *Service) voidActiveChallengesForEntrant(entrantID, reason string) {
	if !s.challengeOn() || entrantID == "" {
		return
	}
	rows, err := s.sb.Select("ladder_challenges",
		"or=(challenger_entrant_id.eq."+store.Q(entrantID)+",challenged_entrant_id.eq."+store.Q(entrantID)+")"+
			"&status=in.(pending,accepted)&select=*")
	if err != nil {
		return
	}
	for _, ch := range rows {
		id := asStr(ch, "id")
		updated, uerr := s.sb.Update("ladder_challenges",
			"id=eq."+store.Q(id)+"&status=in.(pending,accepted)",
			map[string]any{"status": "voided", "resolved_at": now()})
		if uerr != nil || len(updated) == 0 {
			continue
		}
		other := asStr(ch, "challenger_entrant_id")
		if other == entrantID {
			other = asStr(ch, "challenged_entrant_id")
		}
		s.notifyChallenge(other, "challenge", reason, asStr(ch, "league_bracket_id"))
	}
}

// JoinLadder adds the authenticated caller to a division's ladder as an
// account-LINKED entrant (so they can issue + receive challenges), at the
// bottom. Idempotent: returns their existing entrant if already on it. This is
// how real players self-register for a ladder (via the shared join QR/link) —
// the only path that produces the linked entrants challenges require.
func (s *Service) JoinLadder(userID, div string) (model.LadderEntrant, error) {
	if strings.TrimSpace(userID) == "" {
		return model.LadderEntrant{}, ErrForbidden
	}
	if strings.TrimSpace(div) == "" {
		return model.LadderEntrant{}, errors.New("division is required")
	}
	// The division must exist (resolving its league owner proves it).
	if _, err := s.LadderOwner(div); err != nil {
		return model.LadderEntrant{}, err
	}
	pid := s.ensurePlayerForUser(userID)
	if pid == "" {
		return model.LadderEntrant{}, errors.New("please finish setting up your profile first")
	}
	if existing, _ := s.sb.SelectOne("ladder_entrants",
		"league_bracket_id=eq."+store.Q(div)+"&player_id=eq."+store.Q(pid)+"&select=*&limit=1"); existing != nil {
		return mapLadderEntrant(existing), nil
	}
	return s.AddLadderEntrant(div, model.AddLadderEntrantRequest{
		DisplayName: s.displayNameForUser(userID),
		PlayerID:    &pid,
	})
}

// ensurePlayerForUser returns the caller's player id, creating a minimal linked
// player row if the account doesn't have one yet (a brand-new player joining a
// ladder from a QR before ever registering for an event).
func (s *Service) ensurePlayerForUser(userID string) string {
	if pid := s.playerIDForUser(userID); pid != "" {
		return pid
	}
	rows, err := s.sb.Insert("players", map[string]any{
		"user_id":   userID,
		"full_name": s.displayNameForUser(userID),
	})
	if err != nil || len(rows) == 0 {
		return ""
	}
	return asStr(rows[0], "id")
}

// displayNameForUser resolves the account's display name from its profile,
// falling back to "Player".
func (s *Service) displayNameForUser(userID string) string {
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=full_name")
	if err == nil && row != nil {
		if n := strings.TrimSpace(asStr(row, "full_name")); n != "" {
			return n
		}
	}
	return "Player"
}

// playerIDByEmail resolves an ACCOUNT-LINKED player by email (user_id not null),
// so a seeded entrant can be a real, challengeable opponent. "" if none.
func (s *Service) playerIDByEmail(email string) string {
	row, err := s.sb.SelectOne("players",
		"email=eq."+store.Q(strings.ToLower(strings.TrimSpace(email)))+
			"&user_id=not.is.null&select=id&limit=1")
	if err != nil || row == nil {
		return ""
	}
	return asStr(row, "id")
}

// SeedLadderTestEntrants populates a division's ladder for testing: a few demo
// entrants + a couple of recorded results (so W/L + a leapfrog show), PLUS two
// ACCOUNT-LINKED entrants — the caller (at the bottom, so they can challenge up)
// and the other QA account — so the full player-driven challenge flow is
// testable solo (the caller, as league owner, can accept/report via override).
// Owner-gated at the HTTP layer. Returns the number of entrants added.
func (s *Service) SeedLadderTestEntrants(callerUserID, div string) (int, error) {
	if strings.TrimSpace(div) == "" {
		return 0, errors.New("division is required")
	}
	added := 0
	var demoIDs []string
	addDemo := func(name string) {
		if e, err := s.AddLadderEntrant(div, model.AddLadderEntrantRequest{DisplayName: name}); err == nil {
			demoIDs = append(demoIDs, e.ID)
			added++
		}
	}
	addLinked := func(name, playerID string) {
		if playerID == "" {
			return
		}
		// Idempotent: a re-seed must not add a second entrant for the same linked
		// player (two entrants sharing a player_id makes callerEntrantID ambiguous).
		if ex, _ := s.sb.SelectOne("ladder_entrants",
			"league_bracket_id=eq."+store.Q(div)+"&player_id=eq."+store.Q(playerID)+"&select=id&limit=1"); ex != nil {
			return
		}
		pid := playerID
		if _, err := s.AddLadderEntrant(div, model.AddLadderEntrantRequest{DisplayName: name, PlayerID: &pid}); err == nil {
			added++
		}
	}

	for _, nm := range []string{"Sparky Dill", "Gwen Gherkin", "Rick Relish", "Barb Brine"} {
		addDemo(nm)
	}
	// A second linked opponent so a real challenge has an account-linked target.
	addLinked("Krizhia (test)", s.playerIDByEmail("krizhia_roxas29@yahoo.com"))
	// The caller, linked + at the bottom (added last → highest position).
	addLinked("You (test)", s.playerIDForUser(callerUserID))

	// A couple of demo results so the ladder looks alive (a leapfrog + a tie).
	if len(demoIDs) >= 4 {
		_, _ = s.RecordLadderResult(div, model.RecordLadderResultRequest{
			EntrantAID: demoIDs[2], EntrantBID: demoIDs[0], WinnerEntrantID: demoIDs[2]})
		_, _ = s.RecordLadderResult(div, model.RecordLadderResultRequest{
			EntrantAID: demoIDs[1], EntrantBID: demoIDs[3], Tie: true})
	}
	return added, nil
}

// humanDay renders an RFC3339 timestamp as a short local-ish day ("Jul 28").
func humanDay(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return "soon"
	}
	return t.Format("Jan 2")
}
