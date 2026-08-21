package service

import (
	"strconv"
	"strings"
)

// The organization report.
//
// The viewer role is defined as "reporting only" and until now there was no
// report — which made it the one role that couldn't do the thing it was named
// after. It also leaves the paying customer with nothing to forward: a facility
// manager renews the line item they can show their own boss, and a live screen
// inside an app is not that. A file is.
//
// It's the rollup that already exists, written as a spreadsheet: same numbers,
// same quietest-first order, so the file and the screen can never disagree.
// Deliberately NOT a new query — a report that computes its own totals is a
// second source of truth, and the first time it drifts from the screen nobody
// trusts either one.

// OrgReportCSV renders an organization's sites as a spreadsheet.
//
// Readable by anyone who can read the organization, viewers included. That is
// the whole point of the role.
func (s *Service) OrgReportCSV(orgID, callerID string) ([]byte, error) {
	sum, err := s.OrgSummaryFor(orgID, callerID)
	if err != nil {
		return nil, err
	}
	days := strconv.Itoa(sum.SessionsWindowDays)

	var b strings.Builder
	// A title block, because this file gets forwarded and printed with no app
	// around it to say what it is or when it was true.
	b.WriteString(csvCell(sum.Org.Name) + "\n")
	b.WriteString("Sites," + strconv.Itoa(sum.Clubs) + "\n")
	b.WriteString("Members," + strconv.Itoa(sum.Members) + "\n")
	b.WriteString("Sessions (last " + days + " days)," +
		strconv.Itoa(sum.Sessions) + "\n")
	b.WriteString("\n")

	// "Quietest first" is the report's argument, not a sort order, so it's
	// stated in the file rather than left for the reader to notice.
	b.WriteString("Quietest sites first — the ones head office doesn't hear about\n")
	b.WriteString("Site,City,Members,Sessions (last " + days + " days),Last played\n")
	for _, r := range sum.Sites {
		b.WriteString(strings.Join([]string{
			csvCell(r.Name),
			csvCell(r.City),
			strconv.Itoa(r.Members),
			strconv.Itoa(r.Sessions),
			csvCell(lastPlayedCell(r.LastPlayed)),
		}, ","))
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// lastPlayedCell writes the date a human reads, and says so in words when there
// is none. An empty cell reads as missing data; "Never" is a finding — it's the
// site that has never run anything, which is the first one worth a phone call.
func lastPlayedCell(rfc string) string {
	if strings.TrimSpace(rfc) == "" {
		return "Never"
	}
	return dateOnly(rfc)
}
