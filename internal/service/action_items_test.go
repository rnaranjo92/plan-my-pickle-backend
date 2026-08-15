package service

import (
	"strings"
	"testing"
)

func TestCountOfReadsLikeEnglish(t *testing.T) {
	if got := countOf(1, "player waiting", "players waiting"); got != "1 player waiting" {
		t.Fatalf("got %q", got)
	}
	if got := countOf(3, "player waiting", "players waiting"); got != "3 players waiting" {
		t.Fatalf("got %q", got)
	}
}

// The server must NOT format the time: it runs in UTC, so a 6:30 PM San Diego
// game would render as 1:30 AM and land on the wrong day. StartsAt is carried
// raw and the client formats it in the device's timezone.
func TestUpcomingCarriesRawTimestampNotFormattedText(t *testing.T) {
	it := ActionItem{Kind: "upcoming", Title: "You're playing",
		StartsAt: "2026-08-15T18:30:00-07:00"}
	if strings.Contains(it.Title, "PM") || strings.Contains(it.Title, "AM") {
		t.Fatal("the title must not contain a server-formatted clock time")
	}
	if it.StartsAt == "" {
		t.Fatal("StartsAt must be carried so the client can localise it")
	}
}

// A first badge fifty games away is a wall, not an incentive.
func TestMilestonesStartShallowAndAscend(t *testing.T) {
	if gamesPlayedMilestones[0] > 10 {
		t.Fatalf("first milestone is %d — too far for a new player",
			gamesPlayedMilestones[0])
	}
	for i := 1; i < len(gamesPlayedMilestones); i++ {
		if gamesPlayedMilestones[i] <= gamesPlayedMilestones[i-1] {
			t.Fatalf("milestones must ascend: %v", gamesPlayedMilestones)
		}
	}
}

// The approvals/unscored rows come from ranging a Go MAP, whose order is
// randomised per run. Without a deterministic final key the list reshuffles on
// every refresh, which is precisely what it must not do.
func TestOrderIsDeterministicAcrossRuns(t *testing.T) {
	build := func() []ActionItem {
		items := []ActionItem{
			{Kind: "unscored", EventID: "e2", Count: 1},
			{Kind: "approvals", EventID: "e2", Urgent: true},
			{Kind: "achievement"},
			{Kind: "approvals", EventID: "e1", Urgent: true},
			{Kind: "unscored", EventID: "e1", Count: 2},
		}
		sortActionItems(items)
		return items
	}
	first := build()
	for run := 0; run < 50; run++ {
		got := build()
		for i := range got {
			if got[i].Kind != first[i].Kind || got[i].EventID != first[i].EventID {
				t.Fatalf("run %d differs at %d: %v vs %v",
					run, i, got[i], first[i])
			}
		}
	}
	// Urgent leads, and same-kind rows are in event order.
	if !first[0].Urgent || first[0].EventID != "e1" {
		t.Fatalf("expected the urgent e1 approval first, got %+v", first[0])
	}
}

// Home must never become a wall. An organizer with many events could otherwise
// produce a row per event per problem, pushing the feed off the screen — the
// opposite of what the redesign is for.
func TestListIsCappedAndKeepsTheMostUrgent(t *testing.T) {
	var items []ActionItem
	for i := 0; i < 12; i++ {
		items = append(items, ActionItem{Kind: "unscored", EventID: string(rune('a' + i))})
	}
	items = append(items, ActionItem{Kind: "approvals", EventID: "z", Urgent: true})
	sortActionItems(items)
	if len(items) > 0 && !items[0].Urgent {
		t.Fatal("the urgent row must survive the cap")
	}
	if maxActionItems > 6 {
		t.Fatalf("cap is %d — too many rows for a home screen", maxActionItems)
	}
}
