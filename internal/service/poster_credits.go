package service

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Poster credits — the meter for AI poster generation.
//
// Shape decided with Kim (2026-08-19): a pack is **$2.99 for 5 generations**,
// and a **Club subscriber gets 5 included every month**. Five, not three,
// because the research says an organizer needs 2-4 tries before a poster they'll
// actually print — a 3-pack is exactly at that edge, so a bad first style choice
// consumes the whole purchase and the money feels wasted. The extra two renders
// cost ~27c and buy the difference between "fair" and "stingy".
//
// SPEND ORDER: the Club allowance first, then purchased credits. Nobody should
// burn something they paid for while a free monthly allowance sits unused.
//
// INERT until POSTER_CREDITS_ENABLED=true. Everything below is built and
// deployed dark: while the flag is off generation stays free for everyone
// (founding access), and flipping it in Railway starts metering with no deploy.

const (
	// posterPackCredits is what one purchase buys.
	posterPackCredits = 5
	// posterPackCents is the price of that pack.
	posterPackCents = 299
	// posterClubMonthly is the Club tier's included allowance per calendar month.
	posterClubMonthly = 5
)

// posterCreditLocks serializes a user's credit read-then-write. Without it the
// pre-check and the spend are separate reads, so N concurrent generations all
// see the same balance and all decrement to the same value — 15 renders for one
// credit, repeatable hourly. Per USER, so nobody contends with anybody else.
var posterCreditLocks sync.Map // userID -> *sync.Mutex

func lockPosterCredits(userID string) func() {
	muAny, _ := posterCreditLocks.LoadOrStore(userID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// PosterCreditsEnabled reports whether generation is metered at all. Default
// OFF: while founding access lasts, posters are free and this whole file is a
// no-op. Flipping POSTER_CREDITS_ENABLED=true in Railway turns on the meter.
func PosterCreditsEnabled() bool {
	return strings.EqualFold(
		strings.TrimSpace(os.Getenv("POSTER_CREDITS_ENABLED")), "true")
}

// posterCreditsReady reports whether the columns exist. Guarded like every other
// late-added column so the code ships before the migration runs.
func (s *Service) posterCreditsReady() bool {
	return s.columnReady("pmp_profiles", "poster_credits")
}

// PosterCredits is what the studio renders: what you can spend, where it comes
// from, and whether buying more is even possible yet.
type PosterCredits struct {
	// Metered reports whether credits are being enforced at all. False = free.
	Metered bool `json:"metered"`
	// Purchased is the balance bought with money.
	Purchased int `json:"purchased"`
	// ClubRemaining is this month's unused Club allowance (0 for non-Club).
	ClubRemaining int `json:"clubRemaining"`
	// Total is what the organizer can actually spend right now.
	Total int `json:"total"`
	// PackCredits / PackPriceCents describe the thing they'd buy.
	PackCredits    int `json:"packCredits"`
	PackPriceCents int `json:"packPriceCents"`
}

// posterMonthKey is the calendar month a Club allowance belongs to, anchored to
// Pacific so "this month" matches the organizer's month rather than UTC's — a
// UTC boundary would hand somebody a fresh allowance at 5pm on the 31st.
func posterMonthKey(t time.Time) string {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		t = t.In(loc)
	}
	return t.Format("2006-01")
}

// PosterCreditState reads the caller's spendable balance.
func (s *Service) PosterCreditState(userID string) PosterCredits {
	out := PosterCredits{
		Metered:        PosterCreditsEnabled(),
		PackCredits:    posterPackCredits,
		PackPriceCents: posterPackCents,
	}
	if !out.Metered || userID == "" {
		return out
	}
	// Flag on but the migration not run: report UNMETERED rather than a zero
	// balance. Zero would block every organizer — including one who just paid —
	// the moment the flag flips, turning a sequencing mistake into an outage.
	if !s.posterCreditsReady() {
		out.Metered = false
		return out
	}
	if s.posterCompedUnlimited(userID) {
		// Comped: report it as unmetered so the studio shows no balance and no
		// buy button, rather than a confusing "0 credits" beside a working
		// Generate button.
		out.Metered = false
		return out
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+
			"&select=poster_credits,poster_club_used,poster_club_period")
	if err == nil && row != nil {
		out.Purchased = asInt(row, "poster_credits")
		if s.clubPosterAllowance(userID) {
			used := 0
			// A stale period means the allowance has rolled over — treat last
			// month's usage as zero rather than carrying it forward.
			if asStr(row, "poster_club_period") == posterMonthKey(time.Now()) {
				used = asInt(row, "poster_club_used")
			}
			if left := posterClubMonthly - used; left > 0 {
				out.ClubRemaining = left
			}
		}
	} else if err == nil && row == nil && s.clubPosterAllowance(userID) {
		// No profile row yet, but a Club subscriber still has this month's five.
		out.ClubRemaining = posterClubMonthly
	}
	out.Total = out.Purchased + out.ClubRemaining
	return out
}

// clubPosterAllowance reports whether this account gets the Club tier's monthly
// posters.
//
// Gated on subscriptions being LIVE, because ClubPlanActive deliberately returns
// true for EVERYONE while they're off (parity with IsPremium — everything free
// until billing exists). Without this, flipping the poster meter alone would
// hand every user 5 free renders a month, consumed BEFORE anything they paid
// for, so the meter could barely earn.
func (s *Service) clubPosterAllowance(userID string) bool {
	return SubscriptionsEnabled() && s.ClubPlanActive(userID)
}

// ErrNoPosterCredits is returned when the meter is on and the balance is empty.
var ErrNoPosterCredits = errors.New(
	"you're out of poster credits — buy a pack to keep generating")

// spendPosterCredit consumes ONE credit, Club allowance first.
//
// Called only AFTER a successful render: a generation that failed (a timeout, a
// model error) must not cost anybody a credit, and those failures are common
// enough that charging for them would be the loudest complaint this feature
// could produce. The trade is a tiny double-spend window under concurrent
// generations, which is the right way round.
func (s *Service) spendPosterCredit(userID string) {
	if !PosterCreditsEnabled() || userID == "" || !s.posterCreditsReady() ||
		s.posterCompedUnlimited(userID) {
		return
	}
	defer lockPosterCredits(userID)()
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+
			"&select=poster_credits,poster_club_used,poster_club_period")
	if err != nil {
		log.Printf("poster credits: could not read balance for %s: %v", userID, err)
		return
	}
	month := posterMonthKey(time.Now())
	used, purchased := 0, 0
	if row != nil {
		purchased = asInt(row, "poster_credits")
		if asStr(row, "poster_club_period") == month {
			used = asInt(row, "poster_club_used")
		}
	}
	upd := map[string]any{"user_id": userID}
	if s.clubPosterAllowance(userID) && used < posterClubMonthly {
		upd["poster_club_used"] = used + 1
		upd["poster_club_period"] = month
	} else if purchased > 0 {
		upd["poster_credits"] = purchased - 1
	} else {
		return // nothing to take; the pre-check should have stopped this
	}
	if _, err := s.sb.Upsert("pmp_profiles", "user_id", upd); err != nil {
		log.Printf("poster credits: could not spend for %s: %v", userID, err)
	}
}

// posterCompedUnlimited reports whether this account generates without a meter.
//
// COMPED accounts (founders, and beta testers like Marvin who find the bugs on
// live league nights) are exactly the people who must never hit a paywall — and
// they're also the ones generating most while a feature is in beta. `comped` is
// the column Stripe never touches, so this survives billing going live.
func (s *Service) posterCompedUnlimited(userID string) bool {
	if userID == "" || !s.columnReady("pmp_profiles", "comped") {
		return false
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=comped")
	return err == nil && row != nil && asBool(row, "comped")
}

// requirePosterCredit is the gate GeneratePoster/GenerateStudioPoster call
// BEFORE spending money at Gemini.
func (s *Service) requirePosterCredit(userID string) error {
	if !PosterCreditsEnabled() || s.posterCompedUnlimited(userID) {
		return nil
	}
	if st := s.PosterCreditState(userID); st.Total <= 0 {
		return ErrNoPosterCredits
	}
	return nil
}

// StartPosterPackCheckout opens a one-time Checkout for a credit pack. Web-only
// by client convention: selling generations inside the native apps is a
// consumable digital good and would owe Apple/Google 30% (premium.dart already
// routes purchases to the web for exactly this reason).
func (s *Service) StartPosterPackCheckout(
	userID, email, successURL, cancelURL string,
) (string, error) {
	gw, ok := s.stripeGW()
	if !ok {
		return "", ErrPaymentsNotConfigured
	}
	if userID == "" {
		return "", ErrForbidden
	}
	// Never take money for credits that can't be recorded. Without the columns
	// the grant has nowhere to land and the webhook would 200 on a captured
	// payment — $2.99 gone, no credits, no retry. And while the meter is off
	// there is nothing to buy: generation is free for everyone.
	if !s.posterCreditsReady() {
		return "", errors.New(
			"poster credits aren't set up yet — run add_poster_credits.sql")
	}
	if !PosterCreditsEnabled() {
		return "", errors.New("posters are free right now — no credits needed")
	}
	return gw.CreatePlatformCheckout(
		fmt.Sprintf("%s:%d", userID, posterPackCredits),
		"poster_pack",
		posterPackCents,
		"usd",
		fmt.Sprintf("PlanMyPickle — %d AI poster generations", posterPackCredits),
		email, successURL, cancelURL)
}

// GrantPosterCredits applies a paid pack. Called from the Stripe webhook with
// meta "<userID>:<credits>".
//
// Idempotent on the PaymentIntent: Stripe redelivers, and a redelivery must not
// hand out a second pack. The dedup row is written FIRST — if the balance update
// then fails we'd rather under-grant (visible, fixable by hand) than double-grant
// on every retry.
func (s *Service) GrantPosterCredits(meta, dedupKey string) error {
	if !s.posterCreditsReady() {
		log.Printf("poster credits: grant ignored — run add_poster_credits.sql")
		return nil
	}
	parts := strings.Split(meta, ":")
	if len(parts) != 2 {
		return nil
	}
	userID := parts[0]
	credits, _ := strconv.Atoi(parts[1])
	if userID == "" || credits <= 0 {
		return nil
	}
	if dedupKey != "" && s.columnReady("poster_credit_grants", "id") {
		rows, err := s.sb.Insert("poster_credit_grants", map[string]any{
			"payment_ref": dedupKey,
			"user_id":     userID,
			"credits":     credits,
		})
		if err != nil {
			// A failed insert is EITHER a replay (unique violation) OR a
			// transient DB error. Treating both as "already done" loses a paid
			// pack for good — Stripe sees 200 and never retries. So ask which it
			// was: if the row is really there this is a replay and we're done;
			// if not, return the error so Stripe redelivers.
			existing, qerr := s.sb.Select("poster_credit_grants",
				"payment_ref=eq."+store.Q(dedupKey)+"&select=id&limit=1")
			if qerr == nil && len(existing) > 0 {
				return nil // genuine replay
			}
			log.Printf("poster credits: grant insert FAILED for %s (%s) — "+
				"returning error so Stripe retries: %v", userID, dedupKey, err)
			return err
		}
		if len(rows) == 0 {
			return nil
		}
	}
	// The same lock the spend takes: both do read-then-write on poster_credits,
	// so without it a purchase landing during a generation is overwritten by the
	// spend's stale read and the paid pack vanishes.
	defer lockPosterCredits(userID)()
	cur := 0
	if row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=poster_credits"); err == nil &&
		row != nil {
		cur = asInt(row, "poster_credits")
	}
	_, err := s.sb.Upsert("pmp_profiles", "user_id", map[string]any{
		"user_id":        userID,
		"poster_credits": cur + credits,
	})
	if err != nil {
		log.Printf("poster credits: balance update FAILED for %s (+%d): %v",
			userID, credits, err)
	}
	return err
}
