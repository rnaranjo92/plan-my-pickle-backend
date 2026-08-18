package service

import (
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Club-sponsored coach seats.
//
// Premium and the coach plan are separate products, which is right for a coach
// running their own book of business and wrong the moment a FACILITY buys. A
// club that has paid, whose staff are then individually asked to expense a
// second subscription, has been sold a seam rather than a product — and it is
// the seam a Life Time-shaped customer meets on day one.
//
// So: a coach on a paying club's STAFF is covered by that club.
//
// Staff means owner or co-owner — the relationship co-owners introduced. A
// plain member is not staff; a club with forty members would otherwise be
// buying forty coach plans it never agreed to.
//
// WHY THIS IS SAFE TO TAKE AWAY. The coach cap is checked on ADD only, and a
// lapse never removes existing students. So a coach who leaves the club keeps
// every student they already teach and simply can't add more until they pay for
// themselves. No cliff, no support ticket, nobody's programme interrupted —
// which is what makes sponsorship offerable at all. A benefit you cannot
// withdraw kindly is a benefit you cannot offer.

// clubSponsoredCoach reports whether a club with an entitled owner is carrying
// this coach's plan.
//
// Fails CLOSED, unlike most checks here: this is the last thing consulted, and
// everything before it already failed open. Treating a lookup error as "yes"
// would make an unreachable database into free coach plans for everyone.
func (s *Service) clubSponsoredCoach(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	// Their own club first — one query, and the common case for a coach who
	// runs the place they teach at.
	if own, err := s.sb.SelectOne("clubs",
		"owner_id=eq."+store.Q(userID)+"&select=owner_id&limit=1"); err == nil &&
		own != nil {
		if s.IsPremium(userID) {
			return true
		}
	}
	// Otherwise: are they a co-owner somewhere, and is THAT club's owner
	// entitled? The owner's entitlement is what "the club is paying" means —
	// the same rule the season archive uses, so a club's benefits switch on and
	// off together rather than one feature at a time.
	rows, err := s.sb.Select("club_members",
		"user_id=eq."+store.Q(userID)+
			"&role=eq."+ClubRoleCoOwner+"&select=club_id&limit=20")
	if err != nil || len(rows) == 0 {
		return false
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := strings.TrimSpace(asStr(r, "club_id")); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return false
	}
	clubs, err := s.sb.Select("clubs", "id="+store.In(ids)+"&select=owner_id")
	if err != nil {
		return false
	}
	for _, c := range clubs {
		if owner := strings.TrimSpace(asStr(c, "owner_id")); owner != "" &&
			s.IsPremium(owner) {
			return true
		}
	}
	return false
}

// CoachPlanStatus is why a coach can or can't add another student.
//
// It exists because sponsorship is otherwise INVISIBLE: a covered coach simply
// never meets the cap, and a coach who loses cover meets it with a message
// about upgrading that says nothing about the club that was paying. Both of
// those are support tickets — "why am I suddenly limited?" — answerable only by
// someone with database access.
type CoachPlanStatus struct {
	// Active: may add students without limit.
	Active bool `json:"active"`
	// SponsorClub names the club carrying this coach, when one is. Empty when
	// they're paying themselves, comped, or not covered at all.
	SponsorClub string `json:"sponsorClub,omitempty"`
	// FreeStudents is the cap that applies when Active is false.
	FreeStudents int `json:"freeStudents"`
}

// CoachPlanStatusFor answers "can I add another student, and who is paying".
func (s *Service) CoachPlanStatusFor(userID string) CoachPlanStatus {
	out := CoachPlanStatus{
		Active:       s.CoachPlanActive(userID),
		FreeStudents: kFreeCoachStudents,
	}
	if out.Active {
		out.SponsorClub = s.sponsoringClubName(userID)
	}
	return out
}

// sponsoringClubName returns the name of a club carrying this coach, or "".
//
// Best-effort and cosmetic — it decides a sentence, never an entitlement, so a
// failed lookup just means the sentence is less specific.
func (s *Service) sponsoringClubName(userID string) string {
	rows, err := s.sb.Select("club_members",
		"user_id=eq."+store.Q(userID)+
			"&role=eq."+ClubRoleCoOwner+"&select=club_id&limit=20")
	if err != nil {
		return ""
	}
	ids := make([]string, 0, len(rows)+1)
	for _, r := range rows {
		if id := strings.TrimSpace(asStr(r, "club_id")); id != "" {
			ids = append(ids, id)
		}
	}
	if own, oerr := s.sb.Select("clubs",
		"owner_id=eq."+store.Q(userID)+"&select=id,name,owner_id&limit=5"); oerr == nil {
		for _, c := range own {
			if s.IsPremium(asStr(c, "owner_id")) {
				return strings.TrimSpace(asStr(c, "name"))
			}
		}
	}
	if len(ids) == 0 {
		return ""
	}
	clubs, err := s.sb.Select("clubs",
		"id="+store.In(ids)+"&select=name,owner_id")
	if err != nil {
		return ""
	}
	for _, c := range clubs {
		if s.IsPremium(strings.TrimSpace(asStr(c, "owner_id"))) {
			return strings.TrimSpace(asStr(c, "name"))
		}
	}
	return ""
}
