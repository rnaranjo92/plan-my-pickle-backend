package service

import (
	"testing"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

func (s *Service) firstRoundID(t *testing.T, eventID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM rounds WHERE event_id=? LIMIT 1`, eventID).Scan(&id); err != nil {
		t.Fatalf("round id: %v", err)
	}
	return id
}

func TestCollectPayment(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "E", RegistrationFeeCents: 1500})
	reg, _ := s.RegisterPlayer(eid, model.RegisterRequest{FullName: "Ana"})

	ok, err := s.CollectPayment(reg.ID, "stripe")
	if err != nil || !ok {
		t.Fatalf("expected paid, got ok=%v err=%v", ok, err)
	}
	var status string
	s.db.QueryRow(`SELECT payment_status FROM registrations WHERE id=?`, reg.ID).Scan(&status)
	if status != "paid" {
		t.Fatalf("registration not marked paid: %s", status)
	}
	var pstatus string
	var cents int
	s.db.QueryRow(`SELECT status, amount_cents FROM payments WHERE registration_id=?`, reg.ID).Scan(&pstatus, &cents)
	if pstatus != "paid" || cents != 1500 {
		t.Fatalf("payment row wrong: %s %d", pstatus, cents)
	}
}

func TestCollectPaymentFailure(t *testing.T) {
	s := newSvc(t)
	s.Pay = &gateway.MockPayment{ShouldSucceed: false}
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "E", RegistrationFeeCents: 500})
	reg, _ := s.RegisterPlayer(eid, model.RegisterRequest{FullName: "B"})
	ok, _ := s.CollectPayment(reg.ID, "manual")
	if ok {
		t.Fatal("expected failure")
	}
	var status string
	s.db.QueryRow(`SELECT payment_status FROM registrations WHERE id=?`, reg.ID).Scan(&status)
	if status != "pending" {
		t.Fatalf("want pending, got %s", status)
	}
}

func TestCheckInFlows(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "E"})
	reg, _ := s.RegisterPlayer(eid, model.RegisterRequest{FullName: "Ana"})

	// by token (QR)
	got, err := s.CheckInByToken(eid, *reg.CheckInToken)
	if err != nil || got != reg.ID {
		t.Fatalf("token check-in failed: %v %s", err, got)
	}
	var checked int
	var method string
	s.db.QueryRow(`SELECT checked_in, check_in_method FROM registrations WHERE id=?`, reg.ID).Scan(&checked, &method)
	if checked != 1 || method != "qr" {
		t.Fatalf("not checked in via qr: %d %s", checked, method)
	}

	// unknown token
	if _, err := s.CheckInByToken(eid, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStartRoundSendsSms(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "E", Format: "singles", NumCourts: 1})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "A", Phone: "+15550000001"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "B", Phone: "+15550000002"})
	s.GenerateSchedule(eid)

	sent, err := s.StartRound(s.firstRoundID(t, eid))
	if err != nil {
		t.Fatal(err)
	}
	if sent != 2 {
		t.Fatalf("want 2 SMS sent, got %d", sent)
	}
	if mock, ok := s.Sms.(*gateway.MockSms); ok {
		if len(mock.Sent) != 2 {
			t.Fatalf("mock recorded %d", len(mock.Sent))
		}
	}
	var notifs int
	s.db.QueryRow(`SELECT count(*) FROM notifications WHERE event_id=? AND status='sent'`, eid).Scan(&notifs)
	if notifs != 2 {
		t.Fatalf("want 2 sent notifications, got %d", notifs)
	}
}

func TestStartRoundSkipsNoPhone(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "E", Format: "singles", NumCourts: 1})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "A", Phone: "+15550000001"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "NoPhone"})
	s.GenerateSchedule(eid)
	sent, _ := s.StartRound(s.firstRoundID(t, eid))
	if sent != 1 {
		t.Fatalf("want 1, got %d", sent)
	}
}

func TestDuprImport(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "Sanctioned", Format: "singles", DuprSanctioned: true})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "A", DuprID: "D-A"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "B", DuprID: "D-B"})
	s.GenerateSchedule(eid)

	ids := s.poolMatchIDs(t, eid)
	if err := s.RecordScore(ids[0], 11, 6); err != nil {
		t.Fatal(err)
	}
	var pending int
	s.db.QueryRow(`SELECT count(*) FROM dupr_submissions WHERE event_id=? AND status='pending'`, eid).Scan(&pending)
	if pending != 1 {
		t.Fatalf("want 1 pending submission, got %d", pending)
	}
	sum, err := s.SubmitPendingToDupr(eid)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Submitted != 1 || sum.Failed != 0 {
		t.Fatalf("want submitted=1 failed=0, got %+v", sum)
	}
}

func TestDuprImportMissingID(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "Sanctioned", Format: "singles", DuprSanctioned: true})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "A", DuprID: "D-A"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "NoDupr"})
	s.GenerateSchedule(eid)
	ids := s.poolMatchIDs(t, eid)
	s.RecordScore(ids[0], 11, 6)
	sum, _ := s.SubmitPendingToDupr(eid)
	if sum.Failed != 1 || sum.Submitted != 0 {
		t.Fatalf("want failed=1 submitted=0, got %+v", sum)
	}
}

func TestCasualNeverQueuesDupr(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "Casual", Format: "singles"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "A"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "B"})
	s.GenerateSchedule(eid)
	ids := s.poolMatchIDs(t, eid)
	s.RecordScore(ids[0], 11, 6)
	var n int
	s.db.QueryRow(`SELECT count(*) FROM dupr_submissions WHERE event_id=?`, eid).Scan(&n)
	if n != 0 {
		t.Fatalf("casual event should not queue DUPR, got %d", n)
	}
}

func TestDeleteEventCascades(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "Doomed", Format: "singles"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "A"})
	s.RegisterPlayer(eid, model.RegisterRequest{FullName: "B"})
	s.GenerateSchedule(eid)

	if err := s.DeleteEvent(eid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEvent(eid); err != ErrNotFound {
		t.Fatalf("event should be gone, got %v", err)
	}
	for _, tbl := range []string{"brackets", "registrations", "rounds", "matches"} {
		var n int
		s.db.QueryRow("SELECT count(*) FROM "+tbl+" WHERE event_id=?", eid).Scan(&n)
		if n != 0 {
			t.Fatalf("%s not cascaded: %d rows remain", tbl, n)
		}
	}
	if err := s.DeleteEvent("nope"); err != ErrNotFound {
		t.Fatalf("deleting unknown event should be ErrNotFound, got %v", err)
	}
}

func TestSeedDemo(t *testing.T) {
	s := newSvc(t)
	eid, err := s.SeedDemo()
	if err != nil {
		t.Fatal(err)
	}
	regs, _ := s.Registrations(eid)
	if len(regs) != 24 {
		t.Fatalf("want 24 demo players, got %d", len(regs))
	}
	bks, _ := s.GetBrackets(eid)
	if len(bks) != 2 {
		t.Fatalf("want 2 demo brackets, got %d", len(bks))
	}
	// at least one bracket should have populated standings (completed matches)
	total := 0
	for _, b := range bks {
		st, _ := s.Standings(eid, b.ID, true)
		total += len(st)
	}
	if total == 0 {
		t.Fatal("expected demo standings to be populated")
	}
}

func TestRegistrationOutsideRatingFlag(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{
		Name: "E",
		Brackets: []model.BracketInput{
			{Name: "3.0-3.5", MinRating: ratingPtr(3.0), MaxRating: ratingPtr(3.5)},
		},
	})
	bks, _ := s.GetBrackets(eid)
	bid := bks[0].ID
	s.RegisterPlayer(eid, model.RegisterRequest{
		FullName: "InRange", DuprRating: ratingPtr(3.2), BracketID: bid})
	s.RegisterPlayer(eid, model.RegisterRequest{
		FullName: "TooGood", DuprRating: ratingPtr(4.5), BracketID: bid})

	regs, _ := s.Registrations(eid)
	flagged := map[string]bool{}
	for _, r := range regs {
		flagged[r.FullName] = r.OutsideRating
	}
	if flagged["InRange"] {
		t.Fatal("in-range player should NOT be flagged")
	}
	if !flagged["TooGood"] {
		t.Fatal("out-of-range player should be flagged")
	}
}

func TestShirtOrderUpsert(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "E"})
	reg, _ := s.RegisterPlayer(eid, model.RegisterRequest{FullName: "Ana"})

	o, err := s.SaveShirtOrder(reg.ID, model.ShirtRequest{Size: "M", NameOnShirt: "Ana", Number: "7"})
	if err != nil || o.Size != "M" {
		t.Fatalf("save shirt: %v %+v", err, o)
	}
	// update (upsert) — must not create a second row
	if _, err := s.SaveShirtOrder(reg.ID, model.ShirtRequest{Size: "L"}); err != nil {
		t.Fatal(err)
	}
	var count int
	var size string
	s.db.QueryRow(`SELECT count(*) FROM shirt_orders WHERE registration_id=?`, reg.ID).Scan(&count)
	s.db.QueryRow(`SELECT size FROM shirt_orders WHERE registration_id=?`, reg.ID).Scan(&size)
	if count != 1 || size != "L" {
		t.Fatalf("expected 1 upserted order size L, got count=%d size=%s", count, size)
	}
	// size required
	if _, err := s.SaveShirtOrder(reg.ID, model.ShirtRequest{}); err == nil {
		t.Fatal("empty size should error")
	}
	// unknown registration
	if _, err := s.SaveShirtOrder("nope", model.ShirtRequest{Size: "M"}); err != ErrNotFound {
		t.Fatalf("unknown registration should be ErrNotFound, got %v", err)
	}
}

func TestVerifyAdminPasscode(t *testing.T) {
	s := newSvc(t)
	eid, _ := s.CreateEvent(model.CreateEventRequest{Name: "E", AdminPasscode: "1234"})
	if ok, _ := s.VerifyAdminPasscode(eid, "1234"); !ok {
		t.Fatal("correct passcode rejected")
	}
	if ok, _ := s.VerifyAdminPasscode(eid, "0000"); ok {
		t.Fatal("wrong passcode accepted")
	}
	open, _ := s.CreateEvent(model.CreateEventRequest{Name: "Open"})
	if ok, _ := s.VerifyAdminPasscode(open, "whatever"); !ok {
		t.Fatal("no-passcode event should be open")
	}
}
