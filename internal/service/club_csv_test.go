package service

import "testing"

// A name containing a comma is not an edge case — it is "Surname, First", which
// is how half of any imported roster is typed. Getting this wrong shifts every
// column after it and the spreadsheet silently says the wrong thing about
// everyone below.
func TestCsvCell(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Jen Whitfield", "Jen Whitfield"},
		{"  Jen Whitfield  ", "Jen Whitfield"},
		{"Whitfield, Jen", `"Whitfield, Jen"`},
		{`Jen "JW" Whitfield`, `"Jen ""JW"" Whitfield"`},
		{"Jen\nWhitfield", "\"Jen\nWhitfield\""},
		{"", ""},
	}
	for _, c := range cases {
		if got := csvCell(c.in); got != c.want {
			t.Errorf("csvCell(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The spreadsheet is read by a committee member, not a developer.
func TestReadableStatusLeaksNoEnums(t *testing.T) {
	for _, s := range []string{
		ClubStatusActive, ClubStatusSlipping, ClubStatusLapsed, ClubStatusNeverAlong,
	} {
		got := readableStatus(s)
		if got == s {
			t.Errorf("%q was passed through raw — that's a leaked enum in a "+
				"spreadsheet column", s)
		}
		if got == "" {
			t.Errorf("%q has no readable form", s)
		}
	}
}

func TestDateOnly(t *testing.T) {
	if got := dateOnly("2026-08-17T19:00:00Z"); got != "2026-08-17" {
		t.Errorf("got %q", got)
	}
	if got := dateOnly(""); got != "" {
		t.Errorf("got %q for an empty date", got)
	}
}
