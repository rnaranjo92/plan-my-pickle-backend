// Command dupr_smoke exercises the REAL DUPR gateway end-to-end against UAT:
// create a match → update it (corrected score) → delete it. It validates the
// create/update/delete fix without needing the app UI.
//
// Usage (set the partner creds from Railway + the four test players' DUPR ids):
//
//	DUPR_CLIENT_KEY=...  DUPR_CLIENT_SECRET=... \
//	DUPR_TEST_PLAYER_IDS=<p1>,<p2>,<p3>,<p4> \
//	DUPR_CLUB_ID=6364521321 \
//	go run ./cmd/dupr_smoke
//
// Optional: DUPR_BASE_URL (defaults to UAT), DUPR_API_VERSION (defaults v1.0).
// Get each player's DUPR id from their DUPR account profile (the four
// player#@planmypickle.com test accounts).
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
)

func main() {
	ck, cs := os.Getenv("DUPR_CLIENT_KEY"), os.Getenv("DUPR_CLIENT_SECRET")
	if ck == "" || cs == "" {
		log.Fatal("set DUPR_CLIENT_KEY and DUPR_CLIENT_SECRET (the partner creds from Railway)")
	}
	ids := strings.Split(os.Getenv("DUPR_TEST_PLAYER_IDS"), ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	if len(ids) < 4 || ids[0] == "" {
		log.Fatal("set DUPR_TEST_PLAYER_IDS=<p1>,<p2>,<p3>,<p4> (the four DUPR ids)")
	}
	club := os.Getenv("DUPR_CLUB_ID")
	if club == "" {
		club = "6364521321"
	}
	gw := gateway.NewRealDupr(ck, cs,
		os.Getenv("DUPR_BASE_URL"), os.Getenv("DUPR_SSO_BASE"),
		os.Getenv("DUPR_API_VERSION"), club)

	// Pre-check: confirm each DUPR id is known (a bad id would fail the create).
	fmt.Println("== 0) player rating lookups ==")
	for _, id := range ids[:4] {
		r, err := gw.GetPlayerRating(id)
		fmt.Printf("  %s: found=%v doubles=%.2f err=%v\n", id, r.Found, r.Doubles, err)
	}

	matchID := fmt.Sprintf("pmp-smoke-%d", time.Now().Unix()) // our idempotency id
	p := gateway.DuprPayload{
		EventName:    "PMP smoke test",
		MatchID:      matchID,
		MatchDate:    time.Now().Format("2006-01-02"),
		Team1DuprIDs: []string{ids[0], ids[1]},
		Team2DuprIDs: []string{ids[2], ids[3]},
		Games:        [][2]int{{11, 7}},
	}

	fmt.Println("== 1) CREATE (11-7) ==")
	res, err := gw.SubmitMatch(p)
	fmt.Printf("  ok=%v matchCode=%q err=%q goErr=%v\n", res.OK, res.DuprMatchID, res.Error, err)
	if !res.OK || res.DuprMatchID == "" {
		log.Fatal("create failed — stopping (fix the create first)")
	}
	code := res.DuprMatchID
	time.Sleep(2 * time.Second)

	fmt.Println("== 2) UPDATE (corrected to 11-9) ==")
	upd := p
	upd.MatchCode = code
	upd.Games = [][2]int{{11, 9}}
	res2, err := gw.UpdateMatch(upd)
	fmt.Printf("  ok=%v matchCode=%q err=%q goErr=%v\n", res2.OK, res2.DuprMatchID, res2.Error, err)
	time.Sleep(2 * time.Second)

	fmt.Println("== 3) DELETE ==")
	derr := gw.DeleteMatch(code, matchID)
	fmt.Printf("  err=%v\n", derr)

	fmt.Println("\n== summary ==")
	fmt.Printf("create: %v   update: %v   delete: %v\n",
		res.OK, res2.OK, derr == nil)
}
