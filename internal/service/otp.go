package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// Phone OTP — a one-time 6-digit SMS code that a new account verifies to become
// "fully registered". Enforcement (e.g. gating event creation) is gated on the
// SIGNUP_OTP_REQUIRED env flag so it can ship dark and be flipped on only with a
// build that has the OTP UI. Uses the existing Twilio SMS gateway + our own code
// logic (cheapest per message; no Twilio Verify premium).

var (
	ErrNoPhoneOnFile = errors.New("add a phone number first, then request a code")
	ErrOtpNotFound   = errors.New("request a code first")
	ErrOtpExpired    = errors.New("that code expired — request a new one")
	ErrOtpTooMany    = errors.New("too many wrong tries — request a new code")
	ErrOtpWrong      = errors.New("that code isn't right — try again")
	ErrOtpResendSoon = errors.New("please wait a few seconds before requesting another code")
	ErrOtpNoSms      = errors.New("SMS verification isn't available right now")
	// ErrPhoneVerificationRequired is returned by gated actions (e.g. creating an
	// event) when SIGNUP_OTP_REQUIRED is on and the account hasn't verified.
	ErrPhoneVerificationRequired = errors.New("verify your phone number to start organizing")
)

const (
	otpTTL         = 10 * time.Minute
	otpMaxAttempts = 5
	otpResendGap   = 30 * time.Second
)

// SignupOtpRequired gates whether a verified phone is required to "fully
// register" (create events/leagues). Default OFF so it can ship dark.
func SignupOtpRequired() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SIGNUP_OTP_REQUIRED")), "true")
}

// hashOtp stores codes hashed (never plaintext) so a DB leak can't reveal them.
func hashOtp(userID, code string) string {
	sum := sha256.Sum256([]byte(os.Getenv("OTP_SECRET") + "|" + userID + "|" + code))
	return hex.EncodeToString(sum[:])
}

func genOtp() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// PhoneOnFile reports whether the account has any phone number stored — used to
// nudge phone-less accounts (e.g. social sign-in) to add one.
func (s *Service) PhoneOnFile(userID string) bool {
	if userID == "" {
		return false
	}
	row, _ := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=phone")
	return row != nil && strings.TrimSpace(asStr(row, "phone")) != ""
}

// FullyRegistered reports whether the account may organize under the OTP gate: a
// verified phone that is ALSO actually on file. Grandfathered accounts are
// verified but may have no phone — requiring a number on file makes them add one
// (via the verify flow, which collects + verifies it) before they can organize.
func (s *Service) FullyRegistered(userID string) bool {
	if userID == "" {
		return false
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=phone,phone_verified")
	if err != nil || row == nil {
		return false
	}
	return asBool(row, "phone_verified") &&
		strings.TrimSpace(asStr(row, "phone")) != ""
}

// PhoneVerified reports whether the account has a verified phone.
func (s *Service) PhoneVerified(userID string) bool {
	if userID == "" {
		return false
	}
	row, err := s.sb.SelectOne("pmp_profiles",
		"user_id=eq."+store.Q(userID)+"&select=phone_verified")
	return err == nil && row != nil && asBool(row, "phone_verified")
}

// SendPhoneOtp texts a fresh 6-digit code to the account's phone (or phoneOverride
// when the profile has none yet, persisting it), storing it hashed. Rate-limited:
// no resend within otpResendGap.
func (s *Service) SendPhoneOtp(userID, phoneOverride string) error {
	if s.Sms == nil {
		return ErrOtpNoSms
	}
	// Verify the number the user is confirming RIGHT NOW (typed in the flow); fall
	// back to the profile phone only when they didn't provide one. The number is
	// NOT written to the profile here — it's persisted only after a successful
	// verify (VerifyPhoneOtp), so an unverified or someone-else's number can never
	// overwrite the real profile phone, and a code always goes to the typed number.
	phone := strings.TrimSpace(phoneOverride)
	if phone == "" {
		if row, _ := s.sb.SelectOne("pmp_profiles",
			"user_id=eq."+store.Q(userID)+"&select=phone"); row != nil {
			phone = strings.TrimSpace(asStr(row, "phone"))
		}
	}
	if phone == "" || !gateway.SmsReachable(phone) {
		return ErrNoPhoneOnFile
	}
	// Resend throttle.
	if row, _ := s.sb.SelectOne("phone_otps",
		"user_id=eq."+store.Q(userID)+"&select=last_sent_at"); row != nil {
		ls := asStr(row, "last_sent_at")
		if t := parseTS(&ls); !t.IsZero() && time.Since(t) < otpResendGap {
			return ErrOtpResendSoon
		}
	}
	code, err := genOtp()
	if err != nil {
		return err
	}
	if _, err := s.sb.Upsert("phone_otps", "user_id", map[string]any{
		"user_id":      userID,
		"phone":        phone,
		"code_hash":    hashOtp(userID, code),
		"expires_at":   time.Now().Add(otpTTL).UTC().Format(time.RFC3339),
		"attempts":     0,
		"last_sent_at": now(),
	}); err != nil {
		return err
	}
	body := fmt.Sprintf(
		"Your PlanMyPickle verification code is %s. It expires in 10 minutes.", code)
	r, serr := s.Sms.Send(phone, body)
	if serr != nil || !r.OK {
		return errors.New("couldn't send the code — double-check the number")
	}
	return nil
}

// VerifyPhoneOtp checks a code and, on success, marks the account phone-verified
// and clears the pending code. Wrong codes increment the attempt counter.
func (s *Service) VerifyPhoneOtp(userID, code string) error {
	code = strings.TrimSpace(code)
	row, err := s.sb.SelectOne("phone_otps",
		"user_id=eq."+store.Q(userID)+"&select=phone,code_hash,expires_at,attempts")
	if err != nil {
		return err
	}
	if row == nil {
		return ErrOtpNotFound
	}
	exp := asStr(row, "expires_at")
	if t := parseTS(&exp); t.IsZero() || t.Before(time.Now()) {
		return ErrOtpExpired
	}
	if asInt(row, "attempts") >= otpMaxAttempts {
		return ErrOtpTooMany
	}
	if hashOtp(userID, code) != asStr(row, "code_hash") {
		_, _ = s.sb.Update("phone_otps", "user_id=eq."+store.Q(userID),
			map[string]any{"attempts": asInt(row, "attempts") + 1})
		return ErrOtpWrong
	}
	// Success → verified. Persist the number that was actually verified as the
	// profile phone (keeps profile.phone in sync + only ever a VERIFIED number
	// lands there).
	prof := map[string]any{"user_id": userID, "phone_verified": true}
	if p := strings.TrimSpace(asStr(row, "phone")); p != "" {
		prof["phone"] = p
	}
	if _, err := s.sb.Upsert("pmp_profiles", "user_id", prof); err != nil {
		return err
	}
	_ = s.sb.Delete("phone_otps", "user_id=eq."+store.Q(userID))
	return nil
}
