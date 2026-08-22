package service

import (
	"encoding/csv"
	"errors"
	"strings"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Bulk roster import.
//
// Every club we sell to has its members somewhere else first — a spreadsheet, a
// facility export, a sign-up sheet someone typed up. Roster EXPORT has existed
// since clubs shipped; import didn't, so onboarding a club of three hundred
// meant three hundred invitations. That is the difference between an evening
// and a project, and it is the first thing a facility asks about.
//
// Matching is on EMAIL and PHONE only, never on name. Two people called "John
// Smith" are a coin flip, and the cost of getting it wrong is putting a
// stranger on a club's roster — the same reason the player auto-link ignores
// names. Anyone who can't be matched is REPORTED BACK rather than half-created,
// so the owner can send them a join link and knows exactly who's missing.

// clubImportMaxRows bounds the work one paste can ask for. A club roster is
// hundreds, not hundreds of thousands, and an unbounded loop here is an
// unbounded number of queries.
const clubImportMaxRows = 2000

// ClubImportRow is one line that could NOT be added, with the reason, so the
// owner gets a to-do list rather than a number.
type ClubImportRow struct {
	Name   string `json:"name,omitempty"`
	Email  string `json:"email,omitempty"`
	Phone  string `json:"phone,omitempty"`
	Reason string `json:"reason"`
}

// ClubImportResult is what the screen reports after an import.
type ClubImportResult struct {
	// Added is new memberships created by this import.
	Added int `json:"added"`
	// AlreadyIn were matched to an account that is already on the roster.
	// Counted separately because re-importing the same sheet is normal and must
	// read as "nothing to do", not as a failure.
	AlreadyIn int `json:"alreadyIn"`
	// Unmatched is everyone we could not safely identify.
	Unmatched []ClubImportRow `json:"unmatched"`
}

// ImportClubRoster adds people to a club from pasted CSV.
//
// Accepts whatever a club's spreadsheet actually looks like: any column order,
// any of the usual header spellings, and a bare list of emails with no header
// row at all. Requiring a fixed template would mean every club reformats their
// sheet before they can use the feature, which is the moment they give up.
func (s *Service) ImportClubRoster(clubID, callerID, raw string) (ClubImportResult, error) {
	out := ClubImportResult{Unmatched: []ClubImportRow{}}
	if !s.IsClubAdmin(clubID, callerID) {
		return out, ErrForbidden
	}
	rows, err := parseRosterCSV(raw)
	if err != nil {
		return out, err
	}
	if len(rows) == 0 {
		return out, errors.New("no rows found — paste the roster, including its header row")
	}
	if len(rows) > clubImportMaxRows {
		rows = rows[:clubImportMaxRows]
	}

	// Two batched lookups for the whole file, not two per row: a 300-member
	// paste would otherwise be 600 round trips.
	emails := make([]string, 0, len(rows))
	phones := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Email != "" {
			emails = append(emails, r.Email)
		}
		if r.Phone != "" {
			phones = append(phones, r.Phone)
		}
	}
	byEmail := s.usersByEmail(emails)
	byPhone := s.usersByPhone(phones)

	// Who is already here. Existing members are SKIPPED, never re-written: an
	// upsert with role "member" would quietly demote a co-owner, so importing a
	// roster that happens to list your co-owners would strip their access.
	existing := map[string]bool{}
	if mrows, merr := s.sb.SelectAll("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id"); merr == nil {
		for _, m := range mrows {
			existing[asStr(m, "user_id")] = true
		}
	}
	// The owner has no member row at all, so without this they'd be "added" to
	// their own club on every import.
	if owner, oerr := s.clubOwner(clubID); oerr == nil && owner != "" {
		existing[owner] = true
	}

	seen := map[string]bool{} // one line per person, however often they appear
	added := []string{}       // told at the end, in one batch
	for _, r := range rows {
		uid := ""
		if r.Email != "" {
			uid = byEmail[r.Email]
		}
		if uid == "" && r.Phone != "" {
			uid = byPhone[r.Phone]
		}
		if uid == "" {
			reason := "no PlanMyPickle account with that email or phone"
			if r.Email == "" && r.Phone == "" {
				reason = "no email or phone in this row"
			}
			out.Unmatched = append(out.Unmatched, ClubImportRow{
				Name: r.Name, Email: r.Email, Phone: r.Phone, Reason: reason,
			})
			continue
		}
		if existing[uid] || seen[uid] {
			if !seen[uid] {
				out.AlreadyIn++
			}
			seen[uid] = true
			continue
		}
		seen[uid] = true
		if _, uerr := s.sb.Upsert("club_members", "club_id,user_id", map[string]any{
			"club_id": clubID, "user_id": uid, "role": "member",
		}); uerr != nil {
			// One bad row must not abandon the other 299 — report it and carry on.
			out.Unmatched = append(out.Unmatched, ClubImportRow{
				Name: r.Name, Email: r.Email, Phone: r.Phone,
				Reason: "could not be added — try again",
			})
			continue
		}
		out.Added++
		added = append(added, uid)
	}

	// TELL the people who were just added.
	//
	// An import enrolls directly rather than inviting — that is deliberate, it
	// is how a club moves a roster it already has. But being enrolled silently
	// means the club's name appears on your profile and its admins can push to
	// your phone before you know you're in it. The notification is what keeps
	// this an administrative act rather than the app speaking for people: it
	// names the club, and Leave is one tap from the page it links to.
	if len(added) > 0 {
		name := s.clubNameOr(clubID, "a club")
		msg := "You've been added to " + name + "."
		s.recordNotifications(added, "club_added", msg, "club:"+clubID)
		_ = s.sendPush(added, name, msg, "")
	}
	return out, nil
}

// clubNameOr returns the club's name, or fallback if it can't be read. Used
// where a missing name must not fail the operation that is reporting it.
func (s *Service) clubNameOr(clubID, fallback string) string {
	row, err := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=name")
	if err != nil || row == nil {
		return fallback
	}
	if n := strings.TrimSpace(asStr(row, "name")); n != "" {
		return n
	}
	return fallback
}

// usersByEmail maps each given email to an account id, for emails that have one.
func (s *Service) usersByEmail(emails []string) map[string]string {
	out := map[string]string{}
	if len(emails) == 0 {
		return out
	}
	rows, err := s.sb.SelectAll("players",
		"email="+store.In(emails)+"&user_id=not.is.null&select=user_id,email")
	if err != nil {
		return out
	}
	for _, r := range rows {
		e := normalizeEmail(asStr(r, "email"))
		if e == "" {
			continue
		}
		if _, taken := out[e]; !taken { // first match wins, deterministically
			out[e] = asStr(r, "user_id")
		}
	}
	return out
}

// usersByPhone maps each given phone (digits only) to an account id.
//
// Phones are stored however they were typed — "(858) 519-2065", "858-519-2065",
// "+18585192065" — so the stored value is normalized here rather than trusted
// to match the pasted one.
func (s *Service) usersByPhone(phones []string) map[string]string {
	out := map[string]string{}
	if len(phones) == 0 {
		return out
	}
	rows, err := s.sb.SelectAll("players",
		"user_id=not.is.null&phone=not.is.null&select=user_id,phone")
	if err != nil {
		return out
	}
	want := map[string]bool{}
	for _, p := range phones {
		want[p] = true
	}
	for _, r := range rows {
		p := normalizePhone(asStr(r, "phone"))
		if p == "" || !want[p] {
			continue
		}
		if _, taken := out[p]; !taken {
			out[p] = asStr(r, "user_id")
		}
	}
	return out
}

// rosterCSVRow is one parsed line of the pasted sheet.
type rosterCSVRow struct {
	Name  string
	Email string
	Phone string
}

// parseRosterCSV reads a pasted roster into rows.
//
// FieldsPerRecord = -1 because real spreadsheets have ragged lines — a trailing
// comma here, a missing phone there — and rejecting the whole paste over one
// short row would be useless to the person holding the sheet.
func parseRosterCSV(raw string) ([]rosterCSVRow, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if raw == "" {
		return nil, errors.New("nothing pasted")
	}
	rd := csv.NewReader(strings.NewReader(raw))
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true
	records, err := rd.ReadAll()
	if err != nil {
		return nil, errors.New("couldn't read that as a spreadsheet — paste the " +
			"rows as comma-separated text")
	}
	if len(records) == 0 {
		return nil, errors.New("nothing pasted")
	}

	nameAt, emailAt, phoneAt := -1, -1, -1
	start := 0
	if header := records[0]; looksLikeHeader(header) {
		start = 1
		for i, h := range header {
			switch normalizeHeader(h) {
			case "name", "fullname", "member", "player":
				if nameAt < 0 {
					nameAt = i
				}
			case "email", "emailaddress", "e-mail":
				if emailAt < 0 {
					emailAt = i
				}
			case "phone", "phonenumber", "mobile", "cell", "telephone":
				if phoneAt < 0 {
					phoneAt = i
				}
			}
		}
	}

	out := make([]rosterCSVRow, 0, len(records)-start)
	for _, rec := range records[start:] {
		var row rosterCSVRow
		if nameAt >= 0 || emailAt >= 0 || phoneAt >= 0 {
			row.Name = strings.TrimSpace(at(rec, nameAt))
			row.Email = normalizeEmail(at(rec, emailAt))
			row.Phone = normalizePhone(at(rec, phoneAt))
		}
		// No header, or a header we didn't recognise: sniff each cell instead.
		// A pasted column of bare email addresses is the commonest shape of all
		// and has no header at all.
		if row.Email == "" && row.Phone == "" {
			for _, cell := range rec {
				c := strings.TrimSpace(cell)
				switch {
				case strings.Contains(c, "@") && row.Email == "":
					row.Email = normalizeEmail(c)
				case looksLikePhone(c) && row.Phone == "":
					row.Phone = normalizePhone(c)
				case row.Name == "" && c != "":
					row.Name = c
				}
			}
		}
		if row.Name == "" && row.Email == "" && row.Phone == "" {
			continue // blank line
		}
		out = append(out, row)
	}
	return out, nil
}

// looksLikeHeader reports whether the first record names columns rather than
// holding data. A header row containing an "@" is data, not a header.
func looksLikeHeader(rec []string) bool {
	for _, c := range rec {
		if strings.Contains(c, "@") {
			return false
		}
		switch normalizeHeader(c) {
		case "name", "fullname", "member", "player", "email", "emailaddress",
			"e-mail", "phone", "phonenumber", "mobile", "cell", "telephone":
			return true
		}
	}
	return false
}

func normalizeHeader(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, " ", "")))
}

func normalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.Contains(s, "@") {
		return ""
	}
	return s
}

// normalizePhone reduces a phone to comparable digits, dropping a US country
// code so "+1 858 519 2065" and "(858) 519-2065" are the same person.
func normalizePhone(s string) string {
	d := digitsOnly(strings.TrimSpace(s))
	if len(d) == 11 && strings.HasPrefix(d, "1") {
		d = d[1:]
	}
	if len(d) < 7 { // too short to identify anyone
		return ""
	}
	return d
}

func looksLikePhone(s string) bool {
	return normalizePhone(s) != "" && !strings.Contains(s, "@")
}

func at(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return rec[i]
}
