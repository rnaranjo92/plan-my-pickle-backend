package gateway

import "testing"

// ---- MockPayment ----

func TestMockPaymentSuccess(t *testing.T) {
	m := NewMockPayment()
	if m.Live() {
		t.Fatal("mock payment Live() should be false")
	}
	r1, err := m.Charge("reg1", 1500, "usd", "stripe")
	if err != nil {
		t.Fatalf("charge err: %v", err)
	}
	if !r1.OK || r1.Provider != "stripe" || r1.AmountCents != 1500 || r1.Currency != "usd" {
		t.Fatalf("unexpected result: %+v", r1)
	}
	if r1.ProviderRef != "mock_pay_1" {
		t.Errorf("ProviderRef = %q, want mock_pay_1", r1.ProviderRef)
	}
	r2, _ := m.Charge("reg2", 2000, "cad", "paypal")
	if r2.ProviderRef != "mock_pay_2" {
		t.Errorf("seq did not advance: %q", r2.ProviderRef)
	}
	if len(m.Charges) != 2 {
		t.Fatalf("expected 2 recorded charges, got %d", len(m.Charges))
	}
	if m.Charges[1].Provider != "paypal" || m.Charges[1].AmountCents != 2000 {
		t.Errorf("recorded wrong second charge: %+v", m.Charges[1])
	}
}

func TestMockPaymentFailure(t *testing.T) {
	m := &MockPayment{ShouldSucceed: false}
	r, err := m.Charge("reg1", 1000, "usd", "stripe")
	if err != nil {
		t.Fatalf("charge err: %v", err)
	}
	if r.OK {
		t.Error("expected OK=false when ShouldSucceed is false")
	}
	if r.ProviderRef != "" {
		t.Errorf("no ProviderRef expected on failure, got %q", r.ProviderRef)
	}
	// Failed charges are still recorded.
	if len(m.Charges) != 1 {
		t.Fatalf("expected failed charge to be recorded, got %d", len(m.Charges))
	}
}

// ---- MockSms ----

func TestMockSmsFailure(t *testing.T) {
	m := &MockSms{ShouldSucceed: false}
	r, err := m.Send("+15125551234", "hi")
	if err != nil {
		t.Fatalf("send err: %v", err)
	}
	if r.OK {
		t.Error("expected OK=false")
	}
	if len(m.Sent) != 0 {
		t.Errorf("failed send should not be recorded, got %d", len(m.Sent))
	}
}

func TestMockSmsSeq(t *testing.T) {
	m := NewMockSms()
	r1, _ := m.Send("+1", "a")
	r2, _ := m.Send("+2", "b")
	if r1.ProviderRef != "mock_sms_1" || r2.ProviderRef != "mock_sms_2" {
		t.Errorf("seq refs wrong: %q %q", r1.ProviderRef, r2.ProviderRef)
	}
	if len(m.Sent) != 2 {
		t.Fatalf("expected 2 sent, got %d", len(m.Sent))
	}
}

// ---- MockDupr ----

func TestMockDuprSubmitSuccess(t *testing.T) {
	m := NewMockDupr()
	r, err := m.SubmitMatch(DuprPayload{EventID: "e1"})
	if err != nil {
		t.Fatalf("submit err: %v", err)
	}
	if !r.OK || r.DuprMatchID != "mock_dupr_1" {
		t.Fatalf("unexpected: %+v", r)
	}
	if len(m.Submitted) != 1 || m.Submitted[0].EventID != "e1" {
		t.Errorf("submission not recorded: %+v", m.Submitted)
	}
}

func TestMockDuprSubmitFailure(t *testing.T) {
	m := &MockDupr{ShouldSucceed: false}
	r, _ := m.SubmitMatch(DuprPayload{})
	if r.OK || r.Error == "" {
		t.Errorf("expected failure result: %+v", r)
	}
	if len(m.Submitted) != 0 {
		t.Error("failed submit should not be recorded")
	}
}

func TestMockDuprUpdateMatch(t *testing.T) {
	m := NewMockDupr()
	// With a matchCode, echoes it.
	r, errA := m.UpdateMatch(DuprPayload{MatchCode: "ABC"})
	if errA != nil {
		t.Fatalf("dupr err: %v", errA)
	}
	if !r.OK || r.DuprMatchID != "ABC" {
		t.Errorf("with code: %+v", r)
	}
	// Without a code, mints a new mock id.
	r2, errB := m.UpdateMatch(DuprPayload{})
	if errB != nil {
		t.Fatalf("dupr err: %v", errB)
	}
	if !r2.OK || r2.DuprMatchID != "mock_dupr_1" {
		t.Errorf("without code: %+v", r2)
	}
}

func TestMockDuprGetPlayerRating(t *testing.T) {
	m := NewMockDupr()
	empty, _ := m.GetPlayerRating("")
	if empty.Found {
		t.Error("empty id should not be found")
	}
	r, _ := m.GetPlayerRating("XYZ")
	if !r.Found || r.DuprID != "XYZ" || r.Doubles != 3.5 || r.Singles != 3.5 {
		t.Errorf("unexpected rating: %+v", r)
	}
}

func TestMockDuprMiscNoops(t *testing.T) {
	m := NewMockDupr()
	if u, o := m.SsoURL(); u != "" || o != "" {
		t.Errorf("SsoURL should be empty for mock: %q %q", u, o)
	}
	if err := m.RegisterWebhook("https://x"); err != nil {
		t.Errorf("RegisterWebhook err: %v", err)
	}
	if err := m.SubscribeUserRating("X"); err != nil {
		t.Errorf("SubscribeUserRating err: %v", err)
	}
	if err := m.DeleteMatch("code", "id"); err != nil {
		t.Errorf("DeleteMatch err: %v", err)
	}
	members, err := m.ClubMembers("")
	if err != nil {
		t.Fatalf("ClubMembers err: %v", err)
	}
	if len(members) != 2 || members[0].DuprID != "MOCK01" {
		t.Errorf("unexpected members: %+v", members)
	}
}
