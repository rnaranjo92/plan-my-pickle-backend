package service

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// "Player Score Confirm" (Premium add-on): the WINNING side reports a score
// from its token-gated link, the losing side confirms or disputes, and no
// response auto-confirms after the event's timer (PBT's model; DUPR's own
// 72h negative-consent validation is the precedent for auto-accept into
// rated play). Organizer/passcode scoring stays untouched and always wins —
// RecordScore supersedes any open report.
//
// Authorization: a caller proves they're a match participant with their
// REGISTRATION check_in_token (the same token that gates self check-in/pay),
// so only the four (or two) players on the match can act, each via their own
// personal link.

// ErrScoreReportExists means this match already has a report in flight.
var ErrScoreReportExists = errors.New("a score was already reported for this match — confirm or dispute it instead")

// scoreParticipant resolves the caller to their team on a match — via their
// registration token (SMS links, no account needed) OR their signed-in user
// id (the in-app Report/Confirm button; the linked players row identifies
// them). Returns the team (1/2), the event id, and the caller's player id.
func (s *Service) scoreParticipant(matchID, token, callerUserID string) (int, string, string, error) {
	token = strings.TrimSpace(token)
	if token == "" && strings.TrimSpace(callerUserID) == "" {
		return 0, "", "", errors.New("missing link token")
	}
	m, err := s.sb.SelectOne("matches",
		"id=eq."+store.Q(matchID)+"&select=event_id,status")
	if err != nil {
		return 0, "", "", err
	}
	if m == nil {
		return 0, "", "", ErrNotFound
	}
	eventID := asStr(m, "event_id")
	var playerID string
	if token != "" {
		reg, err := s.sb.SelectOne("registrations",
			"event_id=eq."+store.Q(eventID)+"&check_in_token=eq."+store.Q(token)+
				"&select=player_id")
		if err != nil {
			return 0, "", "", err
		}
		if reg == nil {
			return 0, "", "", errors.New("this link doesn't match a player in this event")
		}
		playerID = asStr(reg, "player_id")
	} else {
		// Signed-in path: the caller's linked players row must be registered
		// in this event.
		pl, err := s.sb.SelectOne("players",
			"user_id=eq."+store.Q(callerUserID)+"&select=id")
		if err != nil {
			return 0, "", "", err
		}
		if pl == nil {
			return 0, "", "", errors.New("your account isn't linked to a player in this event")
		}
		playerID = asStr(pl, "id")
	}
	parts, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(matchID)+"&player_id=eq."+store.Q(playerID)+"&select=team")
	if err != nil {
		return 0, "", "", err
	}
	if len(parts) == 0 {
		return 0, "", "", errors.New("you're not a player in this match")
	}
	return asInt(parts[0], "team"), eventID, playerID, nil
}

// scoreReportEvent loads the event fields the feature gates on.
func (s *Service) scoreReportEvent(eventID string) (map[string]any, error) {
	return s.sb.SelectOne("events",
		"id=eq."+store.Q(eventID)+
			"&select=id,name,owner_id,premium_pass,player_scoring,score_confirm_minutes,best_of,points_to_win,win_by")
}

// playerScoringEnabled reports whether the add-on is on AND the event is
// Premium-unlocked (the feature is Premium-gated).
func (s *Service) playerScoringEnabled(ev map[string]any) bool {
	return ev != nil && asBool(ev, "player_scoring") && s.eventPremiumUnlocked(ev)
}

// ScoreReportState is everything the public report/confirm page needs.
type ScoreReportState struct {
	EventName   string   `json:"eventName"`
	Team1       []string `json:"team1"`
	Team2       []string `json:"team2"`
	CallerTeam  int      `json:"callerTeam"`
	MatchStatus string   `json:"matchStatus"` // scheduled | in_progress | completed
	Enabled     bool     `json:"enabled"`     // feature on + premium unlocked
	// The in-flight report, if any.
	HasReport    bool   `json:"hasReport"`
	ReportedTeam int    `json:"reportedTeam,omitempty"`
	Team1Score   int    `json:"team1Score,omitempty"`
	Team2Score   int    `json:"team2Score,omitempty"`
	ReportStatus string `json:"reportStatus,omitempty"` // pending | confirmed | disputed | superseded
	ExpiresAt    string `json:"expiresAt,omitempty"`
	// CanConfirm: the caller is on the OPPOSITE side of a pending report.
	CanConfirm bool `json:"canConfirm"`
}

// GetScoreReportState returns the page state for a participant (token-gated).
func (s *Service) GetScoreReportState(matchID, token, callerUserID string) (ScoreReportState, error) {
	team, eventID, _, err := s.scoreParticipant(matchID, token, callerUserID)
	if err != nil {
		return ScoreReportState{}, err
	}
	ev, err := s.scoreReportEvent(eventID)
	if err != nil {
		return ScoreReportState{}, err
	}
	st := ScoreReportState{CallerTeam: team, EventName: asStr(ev, "name"),
		Enabled: s.playerScoringEnabled(ev)}
	m, err := s.sb.SelectOne("matches", "id=eq."+store.Q(matchID)+"&select=status")
	if err != nil {
		return ScoreReportState{}, err
	}
	st.MatchStatus = asStr(m, "status")
	// Side names for the matchup header.
	parts, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(matchID)+"&select=team,player:players!player_id(full_name)")
	if err != nil {
		return ScoreReportState{}, err
	}
	for _, p := range parts {
		name := ""
		if pl := asMap(p, "player"); pl != nil {
			name = asStr(pl, "full_name")
		}
		if asInt(p, "team") == 1 {
			st.Team1 = append(st.Team1, name)
		} else {
			st.Team2 = append(st.Team2, name)
		}
	}
	if rep, err := s.sb.SelectOne("score_reports",
		"match_id=eq."+store.Q(matchID)+"&select=*"); err == nil && rep != nil {
		st.HasReport = true
		st.ReportedTeam = asInt(rep, "reported_team")
		st.Team1Score = asInt(rep, "team1_score")
		st.Team2Score = asInt(rep, "team2_score")
		st.ReportStatus = asStr(rep, "status")
		st.ExpiresAt = asStr(rep, "expires_at")
		st.CanConfirm = st.ReportStatus == "pending" && team != st.ReportedTeam
	}
	return st, nil
}

// ReportScore records the winning side's score and notifies the losing side
// to confirm or dispute.
func (s *Service) ReportScore(matchID, token, callerUserID string, t1, t2 int) (ScoreReportState, error) {
	team, eventID, _, err := s.scoreParticipant(matchID, token, callerUserID)
	if err != nil {
		return ScoreReportState{}, err
	}
	ev, err := s.scoreReportEvent(eventID)
	if err != nil {
		return ScoreReportState{}, err
	}
	if !s.playerScoringEnabled(ev) {
		return ScoreReportState{}, errors.New("player score reporting isn't enabled for this event")
	}
	// Player Score Confirm captures ONE final score, but a best-of-N event needs
	// the full per-game series — finalizing a single game would fail validation
	// and leave the report stuck pending. Gate the feature off for best-of-N;
	// the organizer enters those results.
	if asInt(ev, "best_of") > 1 {
		return ScoreReportState{}, errors.New("this event is best-of-3 — the organizer enters the series score")
	}
	m, err := s.sb.SelectOne("matches", "id=eq."+store.Q(matchID)+"&select=status")
	if err != nil {
		return ScoreReportState{}, err
	}
	if asStr(m, "status") == "completed" {
		return ScoreReportState{}, errors.New("this match already has a final score")
	}
	if t1 < 0 || t2 < 0 || t1 == t2 {
		return ScoreReportState{}, errors.New("enter the final score (no ties)")
	}
	// Validate the reported score against the event's format NOW — the same rule
	// RecordScore enforces at finalize. Otherwise a plausible-but-illegal score
	// (e.g. 11-10 or 15-11 for an 11/win-by-2 event) would be stored as a report
	// that can never be confirmed, and the auto-confirm ticker would retry it
	// forever. So any report we accept here can actually be finalized.
	ptw := asInt(ev, "points_to_win")
	if ptw <= 0 {
		ptw = 11
	}
	winBy := asInt(ev, "win_by")
	if winBy <= 0 {
		winBy = 2
	}
	if err := validateGame(t1, t2, ptw, winBy); err != nil {
		return ScoreReportState{}, err
	}
	// The WINNING side reports (PBT/UTR convention): the reporter's team must
	// be ahead in the submitted score.
	winner := 1
	if t2 > t1 {
		winner = 2
	}
	if winner != team {
		return ScoreReportState{}, errors.New("the winning side reports the score — ask your opponents to submit it (you'll confirm)")
	}
	minutes := asInt(ev, "score_confirm_minutes")
	if minutes <= 0 {
		minutes = 5
	}
	expires := time.Now().UTC().Add(time.Duration(minutes) * time.Minute).
		Format("2006-01-02T15:04:05.000Z")
	if _, err := s.sb.Insert("score_reports", map[string]any{
		"event_id": eventID, "match_id": matchID, "reported_team": team,
		"team1_score": t1, "team2_score": t2, "expires_at": expires,
	}); err != nil {
		// unique(match_id) → someone already reported.
		if strings.Contains(err.Error(), "duplicate") ||
			strings.Contains(err.Error(), "409") {
			return ScoreReportState{}, ErrScoreReportExists
		}
		return ScoreReportState{}, err
	}
	// Notify the LOSING side to confirm/dispute — each player gets their own
	// token link. Best-effort; the auto-confirm timer covers a missed message.
	go s.notifyScoreConfirm(eventID, matchID, 3-team, t1, t2, minutes)
	return s.GetScoreReportState(matchID, token, callerUserID)
}

// notifyScoreConfirm texts + pushes the given team's players their personal
// confirm links. Best-effort by design (logs, never fails the report).
func (s *Service) notifyScoreConfirm(eventID, matchID string, team, t1, t2, minutes int) {
	parts, err := s.sb.Select("match_participants",
		"match_id=eq."+store.Q(matchID)+"&team=eq."+fmt.Sprint(team)+
			"&select=player:players!player_id(id,phone,user_id)")
	if err != nil {
		log.Printf("score-confirm notify: participants: %v", err)
		return
	}
	var playerIDs []string
	var userIDs []string
	phoneByPlayer := map[string]string{}
	for _, p := range parts {
		pl := asMap(p, "player")
		if pl == nil {
			continue
		}
		pid := asStr(pl, "id")
		playerIDs = append(playerIDs, pid)
		if ph := asStr(pl, "phone"); ph != "" {
			phoneByPlayer[pid] = ph
		}
		if uid := asStr(pl, "user_id"); uid != "" {
			userIDs = append(userIDs, uid)
		}
	}
	if len(playerIDs) == 0 {
		return
	}
	// Each player's personal token link.
	regs, err := s.sb.Select("registrations",
		"event_id=eq."+store.Q(eventID)+"&player_id="+store.In(playerIDs)+
			"&select=player_id,check_in_token")
	if err != nil {
		log.Printf("score-confirm notify: registrations: %v", err)
		return
	}
	tokenByPlayer := map[string]string{}
	for _, r := range regs {
		tokenByPlayer[asStr(r, "player_id")] = asStr(r, "check_in_token")
	}
	scoreTxt := fmt.Sprintf("%d-%d", t1, t2)
	_ = s.sendPush(userIDs, "Confirm your score",
		fmt.Sprintf("Your opponents reported %s. Confirm or dispute within %d min.", scoreTxt, minutes),
		"https://app.planmypickle.com/?event="+eventID)
	for pid, phone := range phoneByPlayer {
		tok := tokenByPlayer[pid]
		if tok == "" {
			continue
		}
		// Short link + terse copy = one SMS segment.
		link := s.ShortLink(fmt.Sprintf(
			"https://app.planmypickle.com/?report=%s&t=%s", matchID, tok))
		body := fmt.Sprintf("PlanMyPickle: opponents reported %s. Confirm or dispute (%dm auto-confirm): %s Reply STOP to opt out.",
			scoreTxt, minutes, link)
		ins, err := s.sb.Insert("notifications", map[string]any{
			"event_id": eventID, "match_id": matchID, "type": "score_confirm",
			"to_address": phone, "body": body,
		})
		if err != nil || len(ins) == 0 {
			continue
		}
		notifID := asStr(ins[0], "id")
		r, err := s.Sms.Send(phone, body)
		st, ref := "failed", any(nil)
		var sentAt any
		if err == nil && r.OK {
			st, ref, sentAt = "sent", r.ProviderRef, now()
		}
		_, _ = s.sb.Update("notifications", "id=eq."+store.Q(notifID),
			map[string]any{"status": st, "provider_ref": ref, "sent_at": sentAt})
	}
}

// ConfirmScore finalizes a pending report (opposite-side participant only) via
// the SAME RecordScore path the organizer uses (bracket advance, DUPR queue…).
func (s *Service) ConfirmScore(matchID, token, callerUserID string) (ScoreReportState, error) {
	team, _, _, err := s.scoreParticipant(matchID, token, callerUserID)
	if err != nil {
		return ScoreReportState{}, err
	}
	rep, err := s.sb.SelectOne("score_reports",
		"match_id=eq."+store.Q(matchID)+"&select=*")
	if err != nil {
		return ScoreReportState{}, err
	}
	if rep == nil {
		return ScoreReportState{}, errors.New("no reported score to confirm")
	}
	if asStr(rep, "status") != "pending" {
		return ScoreReportState{}, errors.New("this score is no longer awaiting confirmation")
	}
	if asInt(rep, "reported_team") == team {
		return ScoreReportState{}, errors.New("the other side confirms the score")
	}
	if err := s.finalizeScoreReport(rep, "confirmed"); err != nil {
		return ScoreReportState{}, err
	}
	return s.GetScoreReportState(matchID, token, callerUserID)
}

// DisputeScore freezes a pending report and flags the organizer (the bracket
// does not advance; the organizer's console entry resolves it).
func (s *Service) DisputeScore(matchID, token, note, callerUserID string) (ScoreReportState, error) {
	team, eventID, _, err := s.scoreParticipant(matchID, token, callerUserID)
	if err != nil {
		return ScoreReportState{}, err
	}
	rep, err := s.sb.SelectOne("score_reports",
		"match_id=eq."+store.Q(matchID)+"&select=*")
	if err != nil {
		return ScoreReportState{}, err
	}
	if rep == nil || asStr(rep, "status") != "pending" {
		return ScoreReportState{}, errors.New("no pending score to dispute")
	}
	if asInt(rep, "reported_team") == team {
		return ScoreReportState{}, errors.New("you reported this score — your opponents confirm or dispute it")
	}
	// Atomically flip pending → disputed: if a concurrent confirm/auto-confirm
	// already finalized (and advanced the bracket) between the SELECT above and
	// here, this matches 0 rows and we lose cleanly instead of stamping a phantom
	// dispute onto an already-advanced match.
	upd, err := s.sb.Update("score_reports",
		"id=eq."+store.Q(asStr(rep, "id"))+"&status=eq.pending",
		map[string]any{"status": "disputed", "dispute_note": strings.TrimSpace(note),
			"resolved_at": now()})
	if err != nil {
		return ScoreReportState{}, err
	}
	if len(upd) == 0 {
		return ScoreReportState{}, errors.New("this score is no longer awaiting confirmation")
	}
	// Flag the organizer (push, best-effort): their console entry is final.
	if ev, err := s.scoreReportEvent(eventID); err == nil && ev != nil {
		_ = s.sendPush([]string{asStr(ev, "owner_id")}, "Score disputed",
			"A reported score was disputed — enter the final score in the app to resolve it.",
			"https://app.planmypickle.com/?e="+eventID)
	}
	return s.GetScoreReportState(matchID, token, callerUserID)
}

// finalizeScoreReport records the reported score through RecordScore and marks
// the report. It first ATOMICALLY CLAIMS the report (pending → status): the
// conditional update only matches while the row is still pending, so a report
// superseded/confirmed/disputed by a concurrent organizer or confirm write
// (between the caller's SELECT and here) is NOT re-recorded over the
// authoritative score. Only the writer that wins the claim proceeds to record.
// Claiming to a non-pending status also keeps supersedeScoreReport (which only
// touches pending/disputed) from flipping it back.
func (s *Service) finalizeScoreReport(rep map[string]any, status string) error {
	id := asStr(rep, "id")
	claimed, err := s.sb.Update("score_reports",
		"id=eq."+store.Q(id)+"&status=eq.pending",
		map[string]any{"status": status, "resolved_at": now()})
	if err != nil {
		return err
	}
	if len(claimed) == 0 {
		return nil // already resolved by a concurrent writer — nothing to do
	}
	matchID := asStr(rep, "match_id")
	if err := s.RecordScore(matchID,
		asInt(rep, "team1_score"), asInt(rep, "team2_score")); err != nil {
		// Recording failed after we claimed the report. Only REOPEN it (→ pending
		// for retry) when the match genuinely didn't get scored: if the match is
		// now 'completed' a real score is on record — this call's partial write,
		// or a competing organizer/confirm write — and reverting would let the
		// ticker re-record the player score over an authoritative one. Leave it
		// resolved in that case (the organizer resolves any advancement hiccup).
		if mm, e := s.sb.SelectOne("matches",
			"id=eq."+store.Q(matchID)+"&select=status"); e == nil && mm != nil &&
			asStr(mm, "status") == "completed" {
			return err
		}
		_, _ = s.sb.Update("score_reports", "id=eq."+store.Q(id),
			map[string]any{"status": "pending", "resolved_at": nil})
		return err
	}
	return nil
}

// AutoConfirmDueScoreReports finalizes pending reports whose confirm window
// has passed (silence = consent — PBT's auto-confirm; DUPR's 72h precedent).
// Runs on the main ticker.
func (s *Service) AutoConfirmDueScoreReports() error {
	rows, err := s.sb.Select("score_reports",
		"status=eq.pending&select=*&order=created_at.asc&limit=100")
	if err != nil {
		return err
	}
	for _, rep := range rows {
		if !dueNow(asStr(rep, "expires_at")) {
			continue
		}
		if err := s.finalizeScoreReport(rep, "confirmed"); err != nil {
			log.Printf("score-confirm auto: match %s: %v", asStr(rep, "match_id"), err)
		}
	}
	return nil
}

// ListScoreReports returns an event's report states keyed for the organizer's
// match cards ({matchId, status, team1Score, team2Score}).
func (s *Service) ListScoreReports(eventID string) ([]map[string]any, error) {
	return s.sb.Select("score_reports",
		"event_id=eq."+store.Q(eventID)+"&select=match_id,status,team1_score,team2_score")
}

// supersedeScoreReport marks any open report on a match superseded — called by
// RecordScore so the organizer's (or a confirm-path) entry always wins and a
// stale pending report can't auto-confirm over it later.
func (s *Service) supersedeScoreReport(matchID string) {
	_, _ = s.sb.Update("score_reports",
		"match_id=eq."+store.Q(matchID)+"&status=in.(pending,disputed)",
		map[string]any{"status": "superseded", "resolved_at": now()})
}

// clampConfirmMinutes bounds the organizer-set auto-confirm window (0 → the
// 5-minute default at report time; cap a day for league-style events).
func clampConfirmMinutes(m int) int {
	if m < 0 {
		return 0
	}
	if m > 1440 {
		return 1440
	}
	return m
}
