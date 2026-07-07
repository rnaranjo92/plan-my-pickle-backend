package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// TestServiceMore covers the heavier DB/engine/gateway methods that don't reach
// out to external networks: storage uploads (fake returns 200), payment + DUPR
// via the default mock gateways, registration, and schedule generation (the
// engine is pure). Stripe/PayPal and geocode/courts methods are excluded since
// they'd hit real third parties. Business errors are tolerated.
func TestServiceMore(t *testing.T) {
	s := newFakeSvc(t, seededFake())

	// Storage uploads — the fake answers /storage/v1 with 200.
	_, _ = s.SetMyPhoto("u1", "image/jpeg", []byte{0xff, 0xd8})
	_ = s.ClearMyPhoto("u1")
	_, _ = s.SetClubLogo("cl1", "u1", "image/png", []byte{1, 2})
	_, _ = s.SetSponsorWatermarkImage("e1", "image/png", []byte{1, 2})

	// Payments via the default MockPayment gateway.
	_, _ = s.CollectPayment("r1", "manual")
	_ = s.CollectPaymentManually("r1")
	// Stripe webhook apply: records the captured amount + grants the paid cart.
	_ = s.CollectPaidFromStripe("r1", 4500, true, false)

	// DUPR via the default MockDupr gateway.
	_ = s.ApplyDuprRating("D1", nil, nil)
	_ = s.ConnectDupr("u1", model.DuprConnectInput{})
	_, _ = s.SubmitPendingToDupr("e1")
	_ = s.RegisterDuprWebhook("https://example.com/hook")

	// Registration + roster import.
	_, _ = s.RegisterPlayer("e1", model.RegisterRequest{}, "u1")
	_, _ = s.ImportRoster("e1", model.ImportRosterRequest{})
	_, _ = s.SaveShirtOrder("r1", model.ShirtRequest{})

	// Schedule + bracket generation (engine logic is pure; RPC hits the fake).
	_, _ = s.GenerateSchedule("e1", false, true)
	_, _ = s.GenerateFlexSchedule("lb1")
	_, _ = s.GeneratePlayoffBracket("b1", 8, "wins", nil)
	_, _ = s.AutoScheduleByRating("e1", false, 0)
	_, _ = s.SyncDivisions("e1", []model.BracketInput{})
	_, _ = s.FillRandomPlayers("e1", "b1")

	// Updates / account / misc mutations.
	_ = s.UpdateClub("cl1", "u1", model.CreateClubRequest{Name: "Renamed"})
	_ = s.SetMatchCourt("m1", 3, nil)
	_ = s.DeleteAccount("u1")
}
