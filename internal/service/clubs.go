package service

import (
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// CreateClub creates a club owned by ownerID and adds the owner as a member with
// the 'owner' role. Premium is enforced by the handler (like createEvent).
func (s *Service) CreateClub(ownerID string, req model.CreateClubRequest) (model.Club, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return model.Club{}, errors.New("club name is required")
	}
	rows, err := s.sb.Insert("clubs", map[string]any{
		"owner_id":     ownerID,
		"name":         name,
		"city":         orNull(strings.TrimSpace(req.City)),
		"description":  orNull(strings.TrimSpace(req.Description)),
		"dupr_club_id": orNull(strings.TrimSpace(req.DuprClubID)),
	})
	if err != nil {
		return model.Club{}, err
	}
	if len(rows) == 0 {
		return model.Club{}, errors.New("club insert returned no row")
	}
	c := mapClub(rows[0])
	_, _ = s.sb.Insert("club_members", map[string]any{
		"club_id": c.ID, "user_id": ownerID, "role": "owner",
	})
	c.IsOwner, c.IsMember, c.MemberCount = true, true, 1
	return c, nil
}

// UpdateClub edits a club's details. Owner-only.
func (s *Service) UpdateClub(clubID, callerID string, req model.CreateClubRequest) error {
	// Co-owners may edit: keeping the club's details current is the day-to-day
	// work they were appointed to do.
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return errors.New("club name is required")
	}
	patch := map[string]any{
		"name":         name,
		"city":         orNull(strings.TrimSpace(req.City)),
		"description":  orNull(strings.TrimSpace(req.Description)),
		"dupr_club_id": orNull(strings.TrimSpace(req.DuprClubID)),
	}
	// Only send the waiver columns once they exist, so an unrun migration leaves
	// the rest of the edit form working instead of failing the whole save.
	if s.clubWaiverReady() {
		patch["requires_waiver"] = req.RequiresWaiver
		patch["waiver_url"] = orNull(normalizeWaiverURL(req.WaiverURL))
	}
	_, err := s.sb.Update("clubs", "id=eq."+store.Q(clubID), patch)
	return err
}

// clubWaiverReady reports whether the waiver columns have been migrated in.
func (s *Service) clubWaiverReady() bool {
	return s.columnReady("clubs", "requires_waiver")
}

// DeleteClub removes a club (owner-only). The schema makes this safe:
// club_members cascade away and events.club_id is SET NULL — the club's
// events live on, just no longer club-branded.
func (s *Service) DeleteClub(clubID, callerID string) error {
	if err := s.requireClubOwner(clubID, callerID); err != nil {
		return err
	}
	return s.sb.Delete("clubs", "id=eq."+store.Q(clubID))
}

// GetClub returns a club for public viewing, with member/event counts and the
// caller's flags (isOwner/isMember; callerID "" for anonymous).
func (s *Service) GetClub(clubID, callerID string) (model.Club, error) {
	row, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=*")
	if err != nil {
		return model.Club{}, err
	}
	if row == nil {
		return model.Club{}, ErrNotFound
	}
	c := mapClub(row)
	c.IsOwner = callerID != "" && callerID == c.OwnerID
	c.MemberCount = s.countRows("club_members", "club_id=eq."+store.Q(clubID), "user_id")
	c.EventCount = s.countRows("events", "club_id=eq."+store.Q(clubID), "id")
	if callerID != "" {
		m, _ := s.sb.SelectOne("club_members",
			"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(callerID)+
				"&select=user_id,role")
		c.IsMember = m != nil
		c.IsCoOwner = m != nil &&
			strings.EqualFold(strings.TrimSpace(asStr(m, "role")), ClubRoleCoOwner)
	}
	s.clubJoinState(&c, callerID)
	return c, nil
}

// MyClubs lists clubs the user owns OR belongs to, newest first.
func (s *Service) MyClubs(userID string) ([]model.Club, error) {
	mem, err := s.sb.Select("club_members", "user_id=eq."+store.Q(userID)+"&select=club_id")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(mem))
	for _, m := range mem {
		if id := asStr(m, "club_id"); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []model.Club{}, nil
	}
	rows, err := s.sb.Select("clubs",
		"id="+store.In(ids)+"&select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Club, 0, len(rows))
	for _, r := range rows {
		c := mapClub(r)
		c.IsOwner = c.OwnerID == userID
		c.IsMember = true
		c.MemberCount = s.countRows("club_members", "club_id=eq."+store.Q(c.ID), "user_id")
		c.EventCount = s.countRows("events", "club_id=eq."+store.Q(c.ID), "id")
		out = append(out, c)
	}
	return out, nil
}

// JoinClub adds the user as a member (idempotent — re-joining is a no-op).
func (s *Service) JoinClub(clubID, userID string) error {
	row, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=id")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	_, err = s.sb.Upsert("club_members", "club_id,user_id", map[string]any{
		"club_id": clubID, "user_id": userID, "role": "member",
	})
	return err
}

// LeaveClub removes the user's membership. The owner can't leave their own club.
func (s *Service) LeaveClub(clubID, userID string) error {
	owner, err := s.clubOwner(clubID)
	if err != nil {
		return err
	}
	if owner == userID {
		return ErrForbidden // owner can't leave; they'd delete the club instead
	}
	return s.sb.Delete("club_members",
		"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(userID))
}

// ClubMembers lists a club's members with display name + photo (batched: two
// queries total — names from linked player rows, photos from pmp_profiles).
func (s *Service) ClubMembers(clubID string) ([]model.ClubMember, error) {
	rows, err := s.sb.Select("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id,role&order=created_at.asc")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		if u := asStr(r, "user_id"); u != "" {
			uids = append(uids, u)
		}
	}
	names := map[string]string{}
	if len(uids) > 0 {
		if prows, err := s.sb.Select("players",
			"user_id="+store.In(uids)+"&select=user_id,full_name"); err == nil {
			for _, p := range prows {
				if n := asStr(p, "full_name"); n != "" {
					names[asStr(p, "user_id")] = n
				}
			}
		}
	}
	photos := s.photosByUser(uids)
	out := make([]model.ClubMember, 0, len(rows))
	for _, r := range rows {
		uid := asStr(r, "user_id")
		out = append(out, model.ClubMember{
			UserID:   uid,
			FullName: names[uid],
			PhotoURL: photos[uid],
			Role:     asStr(r, "role"),
		})
	}
	return out, nil
}

// ClubEvents lists the events that belong to a club, newest first.
func (s *Service) ClubEvents(clubID string) ([]model.Event, error) {
	rows, err := s.sb.Select("events",
		"club_id=eq."+store.Q(clubID)+"&select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapEvent(r))
	}
	return out, nil
}

// ClubLeaderboard aggregates every player's record across ALL of the club's
// (non-team) events into one all-time, by-wins leaderboard — the durable
// "system of record" a rating service can't reconstruct. Public.
//
// IDENTITY: merged by ACCOUNT where a player has one, and by normalized display
// name otherwise.
//
// It used to merge on name alone, described as good-enough for v1 because name
// collisions are rare in a small club. Both halves of that are wrong in the way
// that matters for a record people are asked to trust: two different members
// called Chris B were silently added together into one line, and one member who
// signed up as "Jen W" in March and "Jen Whitfield" in June was split into two.
// Neither is visible as an error — the leaderboard just quietly says something
// untrue about who is winning, which is the one thing this table exists to say.
//
// An account is the only identity the app actually knows, so it wins wherever it
// exists. Name-merging stays for walk-ups and typed-in names, who have no
// account to key on and whose duplicates are a roster problem (fixed with the
// ladder Merge action), not a leaderboard one.
func (s *Service) ClubLeaderboard(clubID string) ([]model.ClubStanding, error) {
	events, err := s.ClubEvents(clubID)
	if err != nil {
		return nil, err
	}
	type agg struct {
		name   string
		wins   int
		losses int
		pf, pa int
		events map[string]bool
	}
	byIdentity := map[string]*agg{}
	// Collect first, aggregate second.
	//
	// The account lookup has to cover every player who appears, and this is a
	// PUBLIC endpoint — so it must be neither one query per row nor a scan of
	// the whole players table. Buffering the standings (a few hundred small
	// structs at most) buys exactly one batched lookup.
	type sourced struct {
		row     model.Standing
		eventID string
	}
	var rows []sourced
	for _, ev := range events {
		if ev.TeamSize > 0 {
			continue // team events rank teams, not players — skip here
		}
		brackets, err := s.GetBrackets(ev.ID)
		if err != nil {
			continue
		}
		for _, b := range brackets {
			// ALL-TIME, deliberately — standingRowsSince with an empty window
			// rather than Standings().
			//
			// Standings() scopes itself to the CURRENT season for a perpetual
			// league (seasonStartFor), which is right for the league's own
			// leaderboard and wrong for this one: a club that rolled a season
			// would watch its all-time record reset to zero, having done the
			// one thing the archive feature encourages. Match history survives
			// a roll — only the live board's window moves — so recomputing
			// from an empty window is the genuine lifetime record.
			//
			// The ranking Standings() adds is discarded here anyway; this
			// aggregates box scores and sorts them itself.
			st, err := s.standingRowsSince(ev.ID, b.ID, "")
			if err != nil {
				continue
			}
			for _, row := range st {
				rows = append(rows, sourced{row: row, eventID: ev.ID})
			}
		}
	}

	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := strings.TrimSpace(r.row.PlayerID); id != "" {
			ids = append(ids, id)
		}
	}
	accounts := s.accountsForPlayers(ids)

	for _, r := range rows {
		key := clubStandingKey(r.row, accounts)
		if key == "" {
			continue
		}
		a := byIdentity[key]
		if a == nil {
			a = &agg{name: r.row.FullName, events: map[string]bool{}}
			byIdentity[key] = a
		}
		// Prefer the fullest name seen for this person. When an account merges
		// "Jen W" with "Jen Whitfield", the leaderboard should show the one
		// their club would recognise.
		if len(strings.TrimSpace(r.row.FullName)) > len(strings.TrimSpace(a.name)) {
			a.name = r.row.FullName
		}
		a.wins += r.row.Wins
		a.losses += r.row.Losses
		a.pf += r.row.PointsFor
		a.pa += r.row.PointsAgainst
		a.events[r.eventID] = true
	}

	out := make([]model.ClubStanding, 0, len(byIdentity))
	for _, a := range byIdentity {
		out = append(out, model.ClubStanding{
			Name:          a.name,
			Wins:          a.wins,
			Losses:        a.losses,
			GamesPlayed:   a.wins + a.losses,
			PointsFor:     a.pf,
			PointsAgainst: a.pa,
			PointDiff:     a.pf - a.pa,
			EventsPlayed:  len(a.events),
		})
	}
	// All-time by wins → win% → games played → point diff → name (stable).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Wins != out[j].Wins {
			return out[i].Wins > out[j].Wins
		}
		wpi := winPct(out[i].Wins, out[i].GamesPlayed)
		wpj := winPct(out[j].Wins, out[j].GamesPlayed)
		if wpi != wpj {
			return wpi > wpj
		}
		if out[i].GamesPlayed != out[j].GamesPlayed {
			return out[i].GamesPlayed > out[j].GamesPlayed
		}
		if out[i].PointDiff != out[j].PointDiff {
			return out[i].PointDiff > out[j].PointDiff
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func winPct(wins, games int) float64 {
	if games == 0 {
		return 0
	}
	return float64(wins) / float64(games)
}

// SetClubLogo uploads a club logo to the public avatars bucket and stamps
// clubs.logo_url. Owner-only. JPEG/PNG up to 5 MB; cache-busted URL.
func (s *Service) SetClubLogo(clubID, callerID, contentType string, data []byte) (string, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return "", err
	}
	var ext string
	switch contentType {
	case "image/jpeg", "image/jpg":
		contentType, ext = "image/jpeg", "jpg"
	case "image/png":
		ext = "png"
	default:
		return "", errors.New("logo must be a JPEG or PNG")
	}
	if len(data) == 0 {
		return "", errors.New("empty logo")
	}
	if len(data) > 5*1024*1024 {
		return "", errors.New("logo too large (max 5 MB)")
	}
	url, err := s.sb.StorageUpload("avatars", "club-"+clubID+"."+ext, contentType, data)
	if err != nil {
		return "", err
	}
	url = fmt.Sprintf("%s?v=%08x", url, crc32.ChecksumIEEE(data))
	if _, err := s.sb.Update("clubs", "id=eq."+store.Q(clubID),
		map[string]any{"logo_url": url}); err != nil {
		return "", err
	}
	return url, nil
}

// OwnsClub reports whether callerID owns the club — used to gate creating an
// event under a club.
func (s *Service) OwnsClub(clubID, callerID string) bool {
	owner, err := s.clubOwner(clubID)
	return err == nil && owner != "" && owner == callerID
}

// --- helpers ---

func (s *Service) clubOwner(clubID string) (string, error) {
	row, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=owner_id")
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return asStr(row, "owner_id"), nil
}

func (s *Service) requireClubOwner(clubID, callerID string) error {
	owner, err := s.clubOwner(clubID)
	if err != nil {
		return err
	}
	if callerID == "" || owner != callerID {
		return ErrForbidden
	}
	return nil
}

// countRows counts matching rows with a minimal projection. Fine for the small
// counts here (a club's members / events).
func (s *Service) countRows(table, query, selectCol string) int {
	rows, err := s.sb.Select(table, query+"&select="+selectCol)
	if err != nil {
		return 0
	}
	return len(rows)
}

func mapClub(m map[string]any) model.Club {
	return model.Club{
		ID:          asStr(m, "id"),
		OwnerID:     asStr(m, "owner_id"),
		Name:        asStr(m, "name"),
		City:        asStr(m, "city"),
		Description: asStr(m, "description"),
		LogoURL:     asStr(m, "logo_url"),
		DuprClubID:  asStr(m, "dupr_club_id"),
		CreatedAt:   asStr(m, "created_at"),
		// Re-normalized on the way OUT as well as in, so rows written before
		// the check existed can't render a hostile href.
		RequiresWaiver: asBool(m, "requires_waiver"),
		WaiverURL:      normalizeWaiverURL(asStr(m, "waiver_url")),
	}
}

// clubStandingKey is the identity a standings row aggregates under: the
// player's ACCOUNT when they have one, otherwise their normalized name.
//
// Pure and separate so the rule can be tested without a club, a database or a
// season of events behind it.
func clubStandingKey(row model.Standing, accounts map[string]string) string {
	if uid := strings.TrimSpace(accounts[strings.TrimSpace(row.PlayerID)]); uid != "" {
		return "account:" + uid
	}
	name := strings.ToLower(strings.TrimSpace(row.FullName))
	if name == "" {
		return "" // nothing to attribute a record to
	}
	return "name:" + name
}

// accountsForPlayers maps player id → account id, for the given players only.
//
// Best-effort: on failure the map is empty and everything falls back to
// name-merging, which is the previous behaviour rather than an error — a club
// should not lose its leaderboard because one lookup timed out.
func (s *Service) accountsForPlayers(playerIDs []string) map[string]string {
	out := map[string]string{}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(playerIDs))
	for _, id := range playerIDs {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return out
	}
	rows, err := s.sb.SelectAll("players",
		"id="+store.In(uniq)+"&user_id=not.is.null&select=id,user_id")
	if err != nil {
		return out
	}
	for _, r := range rows {
		id, uid := strings.TrimSpace(asStr(r, "id")), strings.TrimSpace(asStr(r, "user_id"))
		if id != "" && uid != "" {
			out[id] = uid
		}
	}
	return out
}
