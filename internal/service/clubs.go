package service

import (
	"errors"
	"fmt"
	"hash/crc32"
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
	if err := s.requireClubOwner(clubID, callerID); err != nil {
		return err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return errors.New("club name is required")
	}
	_, err := s.sb.Update("clubs", "id=eq."+store.Q(clubID), map[string]any{
		"name":         name,
		"city":         orNull(strings.TrimSpace(req.City)),
		"description":  orNull(strings.TrimSpace(req.Description)),
		"dupr_club_id": orNull(strings.TrimSpace(req.DuprClubID)),
	})
	return err
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
			"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(callerID)+"&select=user_id")
		c.IsMember = m != nil
	}
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
		"id=in.("+strings.Join(ids, ",")+")&select=*&order=created_at.desc")
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
			"user_id=in.("+strings.Join(uids, ",")+")&select=user_id,full_name"); err == nil {
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

// SetClubLogo uploads a club logo to the public avatars bucket and stamps
// clubs.logo_url. Owner-only. JPEG/PNG up to 5 MB; cache-busted URL.
func (s *Service) SetClubLogo(clubID, callerID, contentType string, data []byte) (string, error) {
	if err := s.requireClubOwner(clubID, callerID); err != nil {
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
	}
}
