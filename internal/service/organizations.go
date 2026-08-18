package service

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Organizations: one layer above clubs, for operators who run several.
//
// A club is a room full of people who know each other. A chain is not — and
// "sign up 180 times with 180 cards" is not how a chain buys anything. What
// this adds is one place to see every site, and staff whose access follows
// their JOB rather than whoever happened to create the club.
//
// Everything here is INERT until add_organizations.sql runs: every entry point
// is gated on the table existing, so a single-club organizer never pays a query
// for a concept they don't use.

// Organization roles. Same three words as clubs where they mean the same
// thing, so there is one mental model rather than two vocabularies.
const (
	OrgRoleOwner  = "owner"  // on the contract: everything, including deleting
	OrgRoleAdmin  = "admin"  // runs sites: manage any club, create events
	OrgRoleViewer = "viewer" // reporting only
)

// Organization is one operator running several clubs.
type Organization struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	OwnerID      string `json:"ownerId"`
	ContactEmail string `json:"contactEmail,omitempty"`
	CreatedAt    string `json:"createdAt,omitempty"`
	// Role is the CALLER's role, when fetched for someone.
	Role string `json:"role,omitempty"`
	// ClubCount is how many sites sit under it.
	ClubCount int `json:"clubCount"`
}

// orgsReady reports whether add_organizations.sql has run. Everything degrades
// to "no organizations exist" until it has, so this ships before the migration.
func (s *Service) orgsReady() bool {
	return s.columnReady("organizations", "id")
}

// OrgRoleFor returns the caller's role in an organization, or "".
//
// The OWNER is not required to have a member row — they are the owner on the
// organizations table, the same shape clubs use — so both are checked.
func (s *Service) OrgRoleFor(orgID, userID string) string {
	if !s.orgsReady() || strings.TrimSpace(userID) == "" {
		return ""
	}
	row, err := s.sb.SelectOne("organizations",
		"id=eq."+store.Q(orgID)+"&select=owner_id")
	if err != nil || row == nil {
		return ""
	}
	if asStr(row, "owner_id") == userID {
		return OrgRoleOwner
	}
	m, merr := s.sb.SelectOne("organization_members",
		"org_id=eq."+store.Q(orgID)+"&user_id=eq."+store.Q(userID)+"&select=role")
	if merr != nil || m == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(asStr(m, "role")))
}

// orgCanManage reports whether a role may RUN sites (not merely read them).
//
// Pure, so the one line that separates "can change a season" from "can look at
// the numbers" is testable — a regional manager who needs the report should not
// be handed the ability to delete one.
func orgCanManage(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case OrgRoleOwner, OrgRoleAdmin:
		return true
	default:
		return false
	}
}

// orgCanRead reports whether a role may see the organization at all.
func orgCanRead(role string) bool {
	return orgCanManage(role) ||
		strings.EqualFold(strings.TrimSpace(role), OrgRoleViewer)
}

// clubOrgAdmin reports whether this user administers the ORGANIZATION that owns
// this club — the inheritance that makes an organization worth having.
//
// Consulted only after the club's own checks fail, and skipped entirely before
// the migration, so the ordinary single-club path costs nothing.
func (s *Service) clubOrgAdmin(clubID, userID string) bool {
	if !s.orgsReady() || strings.TrimSpace(userID) == "" {
		return false
	}
	row, err := s.sb.SelectOne("clubs",
		"id=eq."+store.Q(clubID)+"&select=org_id")
	if err != nil || row == nil {
		return false
	}
	orgID := strings.TrimSpace(asStr(row, "org_id"))
	if orgID == "" {
		return false
	}
	return orgCanManage(s.OrgRoleFor(orgID, userID))
}

// CreateOrganization opens a new organization owned by the caller.
func (s *Service) CreateOrganization(
	ownerID, name, contactEmail string,
) (Organization, error) {
	if !s.orgsReady() {
		return Organization{}, errors.New(
			"organizations aren't enabled yet — run add_organizations.sql")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Organization{}, errors.New("what's the organization called?")
	}
	rows, err := s.sb.Insert("organizations", map[string]any{
		"name":          name,
		"owner_id":      ownerID,
		"contact_email": orNull(strings.TrimSpace(contactEmail)),
	})
	if err != nil {
		return Organization{}, err
	}
	if len(rows) == 0 {
		return Organization{}, errors.New("organization insert returned no row")
	}
	return mapOrganization(rows[0], OrgRoleOwner), nil
}

// MyOrganizations lists the organizations a user owns or works for.
func (s *Service) MyOrganizations(userID string) ([]Organization, error) {
	if !s.orgsReady() || strings.TrimSpace(userID) == "" {
		return []Organization{}, nil
	}
	seen := map[string]bool{}
	out := []Organization{}

	owned, err := s.sb.Select("organizations",
		"owner_id=eq."+store.Q(userID)+"&select=*&order=created_at.desc")
	if err != nil {
		return nil, err
	}
	for _, r := range owned {
		o := mapOrganization(r, OrgRoleOwner)
		seen[o.ID] = true
		out = append(out, o)
	}

	mrows, err := s.sb.Select("organization_members",
		"user_id=eq."+store.Q(userID)+"&select=org_id,role&limit=200")
	if err != nil {
		return out, nil // owned orgs are still worth returning
	}
	roleByOrg := map[string]string{}
	ids := make([]string, 0, len(mrows))
	for _, r := range mrows {
		id := strings.TrimSpace(asStr(r, "org_id"))
		if id == "" || seen[id] {
			continue
		}
		roleByOrg[id] = strings.ToLower(strings.TrimSpace(asStr(r, "role")))
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		if orgs, oerr := s.sb.Select("organizations",
			"id="+store.In(ids)+"&select=*"); oerr == nil {
			for _, r := range orgs {
				out = append(out, mapOrganization(r, roleByOrg[asStr(r, "id")]))
			}
		}
	}
	s.fillOrgClubCounts(out)
	return out, nil
}

// fillOrgClubCounts sets ClubCount for each organization in one query.
func (s *Service) fillOrgClubCounts(orgs []Organization) {
	if len(orgs) == 0 {
		return
	}
	ids := make([]string, 0, len(orgs))
	for _, o := range orgs {
		if o.ID != "" {
			ids = append(ids, o.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := s.sb.SelectAll("clubs", "org_id="+store.In(ids)+"&select=org_id")
	if err != nil {
		return
	}
	n := map[string]int{}
	for _, r := range rows {
		n[asStr(r, "org_id")]++
	}
	for i := range orgs {
		orgs[i].ClubCount = n[orgs[i].ID]
	}
}

// SetOrgRole adds or changes a staff member's role. Owner only — the same rule
// clubs use, and for the same reason: an admin who could appoint admins is an
// owner with extra steps.
func (s *Service) SetOrgRole(orgID, callerID, targetUserID, role string) error {
	if s.OrgRoleFor(orgID, callerID) != OrgRoleOwner {
		return ErrForbidden
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return errors.New("which person?")
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role != OrgRoleAdmin && role != OrgRoleViewer {
		return errors.New("role must be admin or viewer")
	}
	if owner, _ := s.sb.SelectOne("organizations",
		"id=eq."+store.Q(orgID)+"&select=owner_id"); owner != nil &&
		asStr(owner, "owner_id") == targetUserID {
		return errors.New("that's the organization owner")
	}
	_, err := s.sb.Upsert("organization_members", "org_id,user_id", map[string]any{
		"org_id": orgID, "user_id": targetUserID, "role": role,
	})
	return err
}

// RemoveOrgMember revokes someone's access. Owner only.
//
// This is the offboarding path — when somebody leaves the company their access
// should end with their employment, not when a club owner remembers.
func (s *Service) RemoveOrgMember(orgID, callerID, targetUserID string) error {
	if s.OrgRoleFor(orgID, callerID) != OrgRoleOwner {
		return ErrForbidden
	}
	return s.sb.Delete("organization_members",
		"org_id=eq."+store.Q(orgID)+"&user_id=eq."+store.Q(targetUserID))
}

// AttachClub puts a club under an organization, or detaches it with orgID "".
//
// Requires BOTH sides: you must run the organization AND own the club. Either
// alone would let one party help themselves — an org admin quietly absorbing
// somebody else's club, or a club owner attaching to an organization that never
// agreed to carry them.
func (s *Service) AttachClub(clubID, orgID, callerID string) error {
	if !s.orgsReady() {
		return errors.New("organizations aren't enabled yet")
	}
	if !s.OwnsClub(clubID, callerID) {
		return ErrForbidden
	}
	patch := map[string]any{"org_id": nil}
	if orgID = strings.TrimSpace(orgID); orgID != "" {
		if !orgCanManage(s.OrgRoleFor(orgID, callerID)) {
			return ErrForbidden
		}
		patch["org_id"] = orgID
	}
	rows, err := s.sb.Update("clubs", "id=eq."+store.Q(clubID), patch)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrNotFound
	}
	return nil
}

func mapOrganization(r map[string]any, role string) Organization {
	return Organization{
		ID:           asStr(r, "id"),
		Name:         asStr(r, "name"),
		OwnerID:      asStr(r, "owner_id"),
		ContactEmail: asStr(r, "contact_email"),
		CreatedAt:    asStr(r, "created_at"),
		Role:         role,
	}
}

// --- the rollup ---------------------------------------------------------

// OrgSiteRow is one club's line in the corporate view.
type OrgSiteRow struct {
	ClubID   string `json:"clubId"`
	Name     string `json:"name"`
	City     string `json:"city,omitempty"`
	Members  int    `json:"members"`
	Sessions int    `json:"sessions"`
	// LastPlayed is the most recent session at this site (RFC3339), "" if none.
	LastPlayed string `json:"lastPlayed,omitempty"`
}

// OrgSummary is what corporate opens: every site on one page.
type OrgSummary struct {
	Org   Organization `json:"org"`
	Sites []OrgSiteRow `json:"sites"`
	// Totals across the organization.
	Clubs    int `json:"clubs"`
	Members  int `json:"members"`
	Sessions int `json:"sessions"`
	// SessionsWindowDays is what "sessions" counts, so the number can't be read
	// as all-time by someone skimming.
	SessionsWindowDays int `json:"sessionsWindowDays"`
}

// OrgSummaryFor builds the corporate rollup.
//
// Sites are ordered QUIETEST FIRST. A corporate view sorted by size just
// congratulates the biggest club; the reason to look is to find the site that
// has gone quiet, which is the one nobody at head office hears about.
func (s *Service) OrgSummaryFor(orgID, callerID string) (OrgSummary, error) {
	role := s.OrgRoleFor(orgID, callerID)
	if !orgCanRead(role) {
		return OrgSummary{}, ErrForbidden
	}
	row, err := s.sb.SelectOne("organizations", "id=eq."+store.Q(orgID)+"&select=*")
	if err != nil {
		return OrgSummary{}, err
	}
	if row == nil {
		return OrgSummary{}, ErrNotFound
	}
	out := OrgSummary{
		Org:                mapOrganization(row, role),
		Sites:              []OrgSiteRow{},
		SessionsWindowDays: int(clubActivityWindow.Hours() / 24),
	}

	clubs, err := s.sb.SelectAll("clubs",
		"org_id=eq."+store.Q(orgID)+"&select=id,name,city")
	if err != nil {
		return out, err
	}
	if len(clubs) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(clubs))
	for _, c := range clubs {
		if id := asStr(c, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	// Two batched reads for the whole organization rather than two per club:
	// 180 sites would otherwise be 360 queries to render one page.
	members := map[string]int{}
	if rows, merr := s.sb.SelectAll("club_members",
		"club_id="+store.In(ids)+"&select=club_id"); merr == nil {
		for _, r := range rows {
			members[asStr(r, "club_id")]++
		}
	}
	cutoff := time.Now().Add(-clubActivityWindow)
	sessions := map[string]int{}
	last := map[string]time.Time{}
	if rows, eerr := s.sb.SelectAll("events",
		"club_id="+store.In(ids)+"&select=club_id,starts_at"); eerr == nil {
		for _, r := range rows {
			cid := asStr(r, "club_id")
			at, perr := time.Parse(time.RFC3339, strings.TrimSpace(asStr(r, "starts_at")))
			if perr != nil || at.After(time.Now()) {
				continue // undated, or hasn't happened
			}
			if at.After(cutoff) {
				sessions[cid]++
			}
			if cur, ok := last[cid]; !ok || at.After(cur) {
				last[cid] = at
			}
		}
	}

	for _, c := range clubs {
		id := asStr(c, "id")
		r := OrgSiteRow{
			ClubID:   id,
			Name:     asStr(c, "name"),
			City:     asStr(c, "city"),
			Members:  members[id],
			Sessions: sessions[id],
		}
		if t, ok := last[id]; ok {
			r.LastPlayed = t.UTC().Format(time.RFC3339)
		}
		out.Sites = append(out.Sites, r)
		out.Members += r.Members
		out.Sessions += r.Sessions
	}
	out.Clubs = len(out.Sites)

	sort.SliceStable(out.Sites, func(i, j int) bool {
		if out.Sites[i].Sessions != out.Sites[j].Sessions {
			return out.Sites[i].Sessions < out.Sites[j].Sessions // quietest first
		}
		return strings.ToLower(out.Sites[i].Name) <
			strings.ToLower(out.Sites[j].Name)
	})
	return out, nil
}
