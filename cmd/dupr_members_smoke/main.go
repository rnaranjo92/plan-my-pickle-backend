// Command dupr_members_smoke fetches the partner DUPR club's member roster to
// validate the ClubMembers gateway method against UAT.
//
//	DUPR_CLIENT_KEY=... DUPR_CLIENT_SECRET=... DUPR_CLUB_ID=... go run ./cmd/dupr_members_smoke
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
)

func main() {
	ck, cs := os.Getenv("DUPR_CLIENT_KEY"), os.Getenv("DUPR_CLIENT_SECRET")
	if ck == "" || cs == "" {
		log.Fatal("set DUPR_CLIENT_KEY and DUPR_CLIENT_SECRET")
	}
	club := os.Getenv("DUPR_CLUB_ID")
	gw := gateway.NewRealDupr(ck, cs,
		os.Getenv("DUPR_BASE_URL"), os.Getenv("DUPR_SSO_BASE"),
		os.Getenv("DUPR_USER_API_BASE"),
		os.Getenv("DUPR_API_VERSION"), club)

	fmt.Printf("== ClubMembers(club=%q) ==\n", club)
	members, err := gw.ClubMembers(club)
	if err != nil {
		log.Fatalf("ClubMembers failed: %v", err)
	}
	fmt.Printf("  %d members\n", len(members))
	for i, m := range members {
		if i >= 12 {
			fmt.Printf("  … (+%d more)\n", len(members)-12)
			break
		}
		fmt.Printf("  %-8s %-28s s=%s d=%s\n", m.DuprID, m.FullName, m.Singles, m.Doubles)
	}
}
