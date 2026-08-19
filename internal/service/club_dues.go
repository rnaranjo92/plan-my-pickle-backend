package service

import (
	"errors"
	"sync"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Club dues: a season fee, and who has paid it.
//
// A ONE-OFF season fee, not a recurring subscription — and paying is STATUS
// ONLY. Nobody is ever refused a session over a billing state, which is the
// same rule the coach cap and the season archive follow. A club chasing dues
// wants to know who to ask; it does not want the app turning somebody away at
// the door on a Tuesday over a card that expired.
//
// Everything here is inert until add_club_dues.sql runs.

// ClubDuesPeriod is one season's fee.
type ClubDuesPeriod struct {
	ID          string `json:"id"`
	ClubID      string `json:"clubId"`
	Name        string `json:"name"`
	AmountCents int    `json:"amountCents"`
	Currency    string `json:"currency"`
	CreatedAt   string `json:"createdAt,omitempty"`
	ClosedAt    string `json:"closedAt,omitempty"`
	// Paid / Of summarise collection at a glance: "24 of 31".
	Paid int `json:"paid"`
	Of   int `json:"of"`
}

// ClubDuesStatus is one member's standing for the open period.
type ClubDuesStatus struct {
	// Period is nil when the club isn't collecting dues at all, which is the
	// normal case and must not read as "you owe nothing yet".
	Period *ClubDuesPeriod `json:"period,omitempty"`
	Paid   bool            `json:"paid"`
	PaidAt string          `json:"paidAt,omitempty"`
	Method string          `json:"method,omitempty"`
}

// clubDuesLocks serializes SetClubDues per club (see C3).
var clubDuesLocks sync.Map // clubID -> *sync.Mutex

func (s *Service) lockClubDues(clubID string) func() {
	muAny, _ := clubDuesLocks.LoadOrStore(clubID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Service) duesReady() bool {
	return s.columnReady("club_dues_periods", "id")
}

// OpenDuesPeriod returns the club's current period, or nil when it isn't
// collecting. Most clubs never will, and a club that doesn't charge should see
// nothing about money anywhere.
func (s *Service) OpenDuesPeriod(clubID string) *ClubDuesPeriod {
	if !s.duesReady() {
		return nil
	}
	row, err := s.sb.SelectOne("club_dues_periods",
		"club_id=eq."+store.Q(clubID)+"&closed_at=is.null"+
			"&select=*&order=created_at.desc&limit=1")
	if err != nil || row == nil {
		return nil
	}
	p := mapDuesPeriod(row)
	return &p
}

// OpenDuesPeriodFor is OpenDuesPeriod plus the collection counts, for the
// owner's view. Two counts rather than a list: an owner glancing at the club
// wants "24 of 31", and the names are one tap away on the roster.
func (s *Service) OpenDuesPeriodFor(clubID string) *ClubDuesPeriod {
	p := s.OpenDuesPeriod(clubID)
	if p == nil {
		return nil
	}
	// "24 of 31" has to be 24 OF THOSE 31, so both halves are counted over the
	// same people: current members. Counting every payment row against the
	// member count could read "32 of 31" once somebody who paid then left —
	// a number that makes an owner distrust the whole figure, right where they
	// are deciding who still owes them money.
	//
	// Intersected in Go from two whole-set reads rather than filtered in the
	// query: a club with hundreds of members would otherwise put every id into
	// a URL.
	members := s.clubMemberIDs(clubID)
	paid := s.duesPaidUserIDs(p.ID)
	p.Of = len(members)
	for _, uid := range members {
		if paid[uid] {
			p.Paid++
		}
	}
	return p
}

// clubMemberIDs lists the club's current members.
func (s *Service) clubMemberIDs(clubID string) []string {
	rows, err := s.sb.SelectAll("club_members",
		"club_id=eq."+store.Q(clubID)+"&select=user_id")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		if u := strings.TrimSpace(asStr(r, "user_id")); u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// SetClubDues opens a new dues period, closing any period already open.
//
// Closing rather than editing: a club that changes the fee mid-season has
// changed what it charges, and rewriting the old number would make the people
// who already paid look like they paid the new one. The old period keeps its
// payments and stops being the one anybody is asked about.
func (s *Service) SetClubDues(
	clubID, callerID, name string, amountCents int, currency string,
) (ClubDuesPeriod, error) {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return ClubDuesPeriod{}, err
	}
	if !s.duesReady() {
		return ClubDuesPeriod{}, errors.New(
			"dues aren't enabled yet — run add_club_dues.sql")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ClubDuesPeriod{}, errors.New(
			"what's this period called? e.g. \"2026 Season\"")
	}
	if amountCents < 0 {
		return ClubDuesPeriod{}, errors.New("an amount can't be negative")
	}
	currency = strings.ToLower(strings.TrimSpace(currency))
	if currency == "" {
		currency = "usd"
	}
	// Serialize concurrent SetClubDues on one club. Two admins interleaving
	// insert-then-close(neq self) could each close the OTHER's new period and
	// leave the club with zero open periods. The lock makes the pair atomic.
	unlock := s.lockClubDues(clubID)
	defer unlock()

	// INSERT FIRST, then close the old one.
	//
	// Closing first meant a failed insert left the club with NO open period —
	// it silently stopped collecting, and the owner's only clue was the dues
	// card vanishing. This order can briefly leave two periods open instead,
	// which is harmless: OpenDuesPeriod takes the newest, and the close below
	// tidies it on the next attempt even if this one dies here.
	rows, err := s.sb.Insert("club_dues_periods", map[string]any{
		"club_id":      clubID,
		"name":         name,
		"amount_cents": amountCents,
		"currency":     currency,
	})
	if err != nil {
		return ClubDuesPeriod{}, err
	}
	if len(rows) == 0 {
		return ClubDuesPeriod{}, errors.New("dues period insert returned no row")
	}
	created := mapDuesPeriod(rows[0])
	// Close everything that was open BEFORE this one. Scoped by id so the
	// period we just created can't close itself.
	if s.duesReady() {
		_, _ = s.sb.Update("club_dues_periods",
			"club_id=eq."+store.Q(clubID)+"&closed_at=is.null"+
				"&id=neq."+store.Q(created.ID),
			map[string]any{"closed_at": time.Now().UTC().Format(time.RFC3339)})
	}
	return created, nil
}

// CloseClubDues stops collecting, keeping every payment recorded against the
// period. Safe to call when nothing is open.
func (s *Service) CloseClubDues(clubID, callerID string) error {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	if !s.duesReady() {
		return nil
	}
	_, err := s.sb.Update("club_dues_periods",
		"club_id=eq."+store.Q(clubID)+"&closed_at=is.null",
		map[string]any{"closed_at": time.Now().UTC().Format(time.RFC3339)})
	return err
}

// RecordDuesPayment marks a member as paid for the open period.
//
// Manual on purpose: most small clubs take cash, Zelle or a bank transfer, and
// a dues feature that only understands card payments would be recording a
// fiction. Idempotent — marking someone paid twice is a double tap, not a
// second payment.
func (s *Service) RecordDuesPayment(
	clubID, callerID, targetUserID, method, note string,
) error {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	period := s.OpenDuesPeriod(clubID)
	if period == nil {
		return errors.New("this club isn't collecting dues right now")
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return errors.New("which member?")
	}
	// Only a MEMBER can be marked paid. A typo'd/pasted id would otherwise
	// record money against a stranger — invisible in "24 of 31" (which counts
	// members) and unfindable in the UI.
	if !s.isClubMember(clubID, targetUserID) {
		return errors.New("that person isn't a member of this club")
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "manual"
	}
	_, err := s.sb.Upsert("club_dues_payments", "period_id,user_id",
		map[string]any{
			"period_id":    period.ID,
			"user_id":      targetUserID,
			"method":       method,
			"amount_cents": period.AmountCents,
			"note":         orNull(strings.TrimSpace(note)),
			"recorded_by":  callerID,
		})
	return err
}

// UnrecordDuesPayment undoes a payment recorded by mistake.
//
// Necessary rather than tidy: money marked against the wrong member is worse
// than money not marked at all — one person is chased for what they paid while
// another is thanked for what they didn't.
func (s *Service) UnrecordDuesPayment(clubID, callerID, targetUserID string) error {
	if err := s.requireClubAdmin(clubID, callerID); err != nil {
		return err
	}
	period := s.OpenDuesPeriod(clubID)
	if period == nil {
		return errors.New("this club isn't collecting dues right now")
	}
	return s.sb.Delete("club_dues_payments",
		"period_id=eq."+store.Q(period.ID)+
			"&user_id=eq."+store.Q(strings.TrimSpace(targetUserID)))
}

// isClubMember reports whether this user is on the club's member list. The
// owner is inserted as a member at creation, so this covers them too.
//
// Fails CLOSED, unlike most best-effort lookups here: it gates who gets asked
// for money, and a database blip must not turn into a stranger being invited to
// pay a club's dues.
func (s *Service) isClubMember(clubID, userID string) bool {
	if strings.TrimSpace(userID) == "" {
		return false
	}
	row, err := s.sb.SelectOne("club_members",
		"club_id=eq."+store.Q(clubID)+"&user_id=eq."+store.Q(userID)+
			"&select=user_id")
	return err == nil && row != nil
}

// MyDuesStatus is what a member sees about their own standing.
//
// MEMBERS ONLY. A club page is open to anyone with the link, so without this
// check a passer-by was shown "2026 Season dues · $60" with a Pay button for a
// club they had never joined — and paying it would have taken their money and
// given them nothing, since dues are recorded against a membership they don't
// have. Joining is free and one tap; that's the step that comes first.
func (s *Service) MyDuesStatus(clubID, userID string) ClubDuesStatus {
	out := ClubDuesStatus{}
	if strings.TrimSpace(userID) == "" {
		return out
	}
	if !s.isClubMember(clubID, userID) {
		return out
	}
	period := s.OpenDuesPeriod(clubID)
	if period == nil {
		return out
	}
	out.Period = period
	row, err := s.sb.SelectOne("club_dues_payments",
		"period_id=eq."+store.Q(period.ID)+"&user_id=eq."+store.Q(userID)+
			"&select=paid_at,method")
	if err != nil || row == nil {
		return out
	}
	out.Paid = true
	out.PaidAt = asStr(row, "paid_at")
	out.Method = asStr(row, "method")
	return out
}

// duesPaidUserIDs returns everyone who has paid the open period, for the roster.
func (s *Service) duesPaidUserIDs(periodID string) map[string]bool {
	out := map[string]bool{}
	if periodID == "" {
		return out
	}
	rows, err := s.sb.SelectAll("club_dues_payments",
		"period_id=eq."+store.Q(periodID)+"&select=user_id")
	if err != nil {
		return out
	}
	for _, r := range rows {
		if u := strings.TrimSpace(asStr(r, "user_id")); u != "" {
			out[u] = true
		}
	}
	return out
}

func mapDuesPeriod(r map[string]any) ClubDuesPeriod {
	return ClubDuesPeriod{
		ID:          asStr(r, "id"),
		ClubID:      asStr(r, "club_id"),
		Name:        asStr(r, "name"),
		AmountCents: asInt(r, "amount_cents"),
		Currency:    asStr(r, "currency"),
		CreatedAt:   asStr(r, "created_at"),
		ClosedAt:    asStr(r, "closed_at"),
	}
}

// --- paying online ---------------------------------------------------------

// duesRef encodes a dues payment for Stripe metadata: "periodId:userId".
//
// The same shape class packs already use. Dues have no registration row to hang
// a payment off, so the reference travels in metadata and comes back on the
// webhook.
func duesRef(periodID, userID string) string {
	return strings.TrimSpace(periodID) + ":" + strings.TrimSpace(userID)
}

// parseDuesRef splits a reference back into its parts. Returns empty strings
// when the reference is malformed, which the caller treats as "not a dues
// payment" rather than guessing.
func parseDuesRef(ref string) (periodID, userID string) {
	parts := strings.SplitN(strings.TrimSpace(ref), ":", 2)
	if len(parts) != 2 {
		return "", ""
	}
	periodID, userID = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if periodID == "" || userID == "" {
		return "", ""
	}
	return periodID, userID
}

// StartDuesCheckout opens a Stripe Checkout for a member's own dues.
//
// The money goes to the CLUB — a destination charge on the owner's connected
// account, the same path an entry fee takes — because these are the club's
// dues, not ours. Which also means a club whose owner hasn't finished payout
// onboarding simply can't collect online, and is told so rather than being sent
// to a checkout that would fail.
func (s *Service) StartDuesCheckout(
	clubID, userID, successURL, cancelURL string,
) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", ErrForbidden
	}
	// Checked again here, not just in MyDuesStatus: this is the endpoint that
	// moves money, and it must not depend on the card that offers it having
	// been rendered honestly.
	if !s.isClubMember(clubID, userID) {
		return "", errors.New("join the club first — then you can pay its dues")
	}
	period := s.OpenDuesPeriod(clubID)
	if period == nil {
		return "", errors.New("this club isn't collecting dues right now")
	}
	if period.AmountCents <= 0 {
		return "", errors.New("these dues are free — nothing to pay")
	}
	// Already paid? Say so instead of taking the money twice.
	if st := s.MyDuesStatus(clubID, userID); st.Paid {
		return "", errors.New("you've already paid for this period")
	}
	owner, err := s.clubOwner(clubID)
	if err != nil {
		return "", err
	}
	orow, err := s.organizerPaymentRow(owner)
	if err != nil {
		return "", err
	}
	if orow == nil {
		return "", ErrOrganizerNotConnected
	}
	accountID := asStr(orow, "stripe_account_id")
	if accountID == "" || !asBool(orow, "charges_enabled") {
		return "", ErrOrganizerNotConnected
	}
	club, _ := s.sb.SelectOne("clubs", "id=eq."+store.Q(clubID)+"&select=name")
	name := strings.TrimSpace(asStr(club, "name"))
	if name == "" {
		name = "Club"
	}
	currency := strings.ToLower(strings.TrimSpace(period.Currency))
	if currency == "" {
		currency = "usd"
	}
	surcharge := processingSurchargeCents(period.AmountCents, currency)
	return gw.CreateCheckoutSession(gateway.CheckoutParams{
		DuesRef:             duesRef(period.ID, userID),
		AmountCents:         period.AmountCents,
		Currency:            currency,
		ProductName:         name + " — " + period.Name,
		DestinationAccount:  accountID,
		ApplicationFeeCents: platformFeeCents(period.AmountCents, currency) + surcharge,
		ServiceFeeCents:     surcharge,
		SuccessURL:          successURL,
		CancelURL:           cancelURL,
	})
}

// markDuesPaidFromWebhook records a paid membership when Stripe confirms it.
//
// Idempotent by the table's primary key (period, user), so a redelivered
// webhook writes the same row rather than a second payment. Records what Stripe
// actually captured rather than the period's price, because the two can differ
// if the fee changed between checkout and completion.
func (s *Service) markDuesPaidFromWebhook(ref string, amountCents int) error {
	periodID, userID := parseDuesRef(ref)
	if periodID == "" {
		return nil // not a dues payment we can attribute; don't guess
	}
	if !s.duesReady() {
		return nil
	}
	row := map[string]any{
		"period_id": periodID,
		"user_id":   userID,
		"method":    "stripe",
	}
	if amountCents > 0 {
		row["amount_cents"] = amountCents
	}
	_, err := s.sb.Upsert("club_dues_payments", "period_id,user_id", row)
	return err
}
