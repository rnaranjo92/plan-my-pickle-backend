package service

import (
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Sibling clubs: the other branches under the same organization.
//
// A chain runs one club per SITE — each branch has its own members, its own
// nights and its own roster, which is what a club is for. What a branch manager
// also needs is to see what the other branches are doing: who's running a
// league on Thursday, which site has gone quiet, what the one across town tried
// that worked.
//
// Deliberately NOT a way to create clubs. Branches are provisioned by us, so
// this only ever LISTS what already exists.

// SiblingClubs lists the other clubs under this club's organization.
//
// Requires the caller to have a role in that ORGANIZATION. Being staff at one
// branch is not, by itself, permission to read another branch's numbers — the
// organization is what says these sites belong together and who may see across
// them. Returns empty (not an error) when the club has no organization or the
// caller isn't in it: this feeds a section that simply doesn't appear.
func (s *Service) SiblingClubs(clubID, callerID string) ([]model.Club, error) {
	row, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=org_id")
	if err != nil || row == nil {
		return []model.Club{}, nil
	}
	orgID := asStr(row, "org_id")
	if orgID == "" {
		return []model.Club{}, nil
	}
	if !orgCanRead(s.OrgRoleFor(orgID, callerID)) {
		return []model.Club{}, nil
	}
	rows, err := s.sb.Select("clubs",
		"org_id=eq."+store.Q(orgID)+"&id=neq."+store.Q(clubID)+
			"&select=id,name,city,logo_url,brand_color&order=name.asc")
	if err != nil {
		return nil, err
	}
	out := make([]model.Club, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.Club{
			ID:         asStr(r, "id"),
			Name:       asStr(r, "name"),
			City:       asStr(r, "city"),
			LogoURL:    asStr(r, "logo_url"),
			BrandColor: asStr(r, "brand_color"),
		})
	}
	return out, nil
}
