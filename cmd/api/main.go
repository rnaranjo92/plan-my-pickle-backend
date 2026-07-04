// Command api runs the PlanMyPickle backend HTTP server.
package main

import (
	"bufio"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/api"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/gateway"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/service"
	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

func main() {
	loadDotEnv(".env") // optional local config (SUPABASE_URL, SUPABASE_SERVICE_KEY, …)

	addr := env("PMP_ADDR", ":8080")
	// PaaS platforms (Railway/Heroku/Render) inject $PORT and expect the app to
	// bind to it. Honor it unless PMP_ADDR was set explicitly.
	if _, set := os.LookupEnv("PMP_ADDR"); !set {
		if port := os.Getenv("PORT"); port != "" {
			addr = ":" + port
		}
	}

	// Supabase is the sole data store — refuse to start without it. (The anon
	// key is blocked by RLS; the backend needs the service_role key.)
	if !store.NewClient().Ready() {
		if os.Getenv("SUPABASE_URL") != "" && os.Getenv("SUPABASE_SERVICE_KEY") == "" {
			log.Fatal("Supabase: SUPABASE_URL set but SUPABASE_SERVICE_KEY missing — " +
				"the backend needs the service_role key (the anon key is blocked by RLS)")
		}
		log.Fatal("Supabase: not configured — set SUPABASE_URL + SUPABASE_SERVICE_KEY")
	}
	log.Printf("Supabase: configured (%s)", os.Getenv("SUPABASE_URL"))

	svc := service.New()
	// Real online payments (Stripe Connect) only when STRIPE_SECRET_KEY is set;
	// otherwise the mock stays in place (Live()=false → fee events stay "pending"
	// until the organizer confirms via mark-paid). The webhook secret verifies
	// incoming Stripe callbacks. Secrets live in the platform env, never code.
	if sk := os.Getenv("STRIPE_SECRET_KEY"); sk != "" {
		svc.Pay = gateway.NewStripeGateway(sk, os.Getenv("STRIPE_WEBHOOK_SECRET"))
		if os.Getenv("STRIPE_WEBHOOK_SECRET") == "" {
			log.Printf("Payments: Stripe configured — WARNING STRIPE_WEBHOOK_SECRET unset, webhooks will be rejected")
		} else {
			log.Printf("Payments: Stripe Connect configured (destination charges)")
		}
	} else {
		log.Printf("Payments: mock — set STRIPE_SECRET_KEY (+ STRIPE_WEBHOOK_SECRET) to take real payments")
	}
	// Real SMS (game-starting alerts) only when Twilio is configured; otherwise
	// the mock records notifications without sending. from = E.164 number or a
	// Messaging Service SID (MG…). Secrets live in the platform env, never code.
	if sid, tok, from := os.Getenv("TWILIO_ACCOUNT_SID"), os.Getenv("TWILIO_AUTH_TOKEN"), os.Getenv("TWILIO_FROM"); sid != "" && tok != "" && from != "" {
		svc.Sms = gateway.NewTwilioSms(sid, tok, from)
		log.Printf("SMS: Twilio configured (from %s)", from)
	} else {
		log.Printf("SMS: mock — set TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_FROM to send real texts")
	}
	// Real DUPR (rating verification + sanctioned-match submission) only when the
	// partner Client Key + Secret are set; otherwise the mock stands in. Base URL
	// defaults to UAT — set DUPR_BASE_URL=https://prod.mydupr.com/api for production.
	// DUPR_API_VERSION / DUPR_CLUB_ID are optional. Secrets live in the env, never code.
	if ck, cs := os.Getenv("DUPR_CLIENT_KEY"), os.Getenv("DUPR_CLIENT_SECRET"); ck != "" && cs != "" {
		svc.Dupr = gateway.NewRealDupr(ck, cs,
			os.Getenv("DUPR_BASE_URL"), os.Getenv("DUPR_SSO_BASE"),
			// User-token API host (entitlements + token refresh). Defaults to
			// UAT api.uat.dupr.gg — set DUPR_USER_API_BASE=https://api.dupr.gg for prod.
			os.Getenv("DUPR_USER_API_BASE"),
			os.Getenv("DUPR_API_VERSION"), os.Getenv("DUPR_CLUB_ID"))
		log.Printf("DUPR: partner API configured")
		// Register our rating webhook (best-effort; idempotent by URL). The
		// receiver is fail-closed on DUPR_WEBHOOK_SECRET, passed as ?token= so DUPR
		// echoes it back — without the secret we don't register (it'd be rejected).
		if sec := os.Getenv("DUPR_WEBHOOK_SECRET"); sec != "" {
			base := os.Getenv("DUPR_WEBHOOK_URL")
			if base == "" {
				base = "https://api.planmypickle.com/dupr/webhook"
			}
			hook := base + "?token=" + url.QueryEscape(sec)
			go func() {
				if err := svc.RegisterDuprWebhook(hook); err != nil {
					log.Printf("DUPR: webhook register failed (non-fatal): %v", err)
				} else {
					log.Printf("DUPR: rating webhook registered")
				}
			}()
		} else {
			log.Printf("DUPR: webhook NOT registered — set DUPR_WEBHOOK_SECRET to enable rating updates")
		}
		// Background reconciler: retry transient DUPR submission failures on a
		// backoff so a hiccup self-heals without the organizer re-importing.
		go func() {
			ticker := time.NewTicker(2 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				if err := svc.ReconcileDuprSubmissions(); err != nil {
					log.Printf("DUPR: submission reconcile failed: %v", err)
				}
			}
		}()
	} else {
		log.Printf("DUPR: mock — set DUPR_CLIENT_KEY, DUPR_CLIENT_SECRET to verify ratings + submit matches")
	}

	handler := api.NewServer(svc)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		// Cap request headers (DoS hardening); the default is 1 MB but we set it
		// explicitly. Bodies are bounded per-request in decode() via MaxBytesReader.
		MaxHeaderBytes: 1 << 20,
	}
	log.Printf("PlanMyPickle API listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv loads KEY=VALUE lines from a .env file into the process
// environment, without overriding variables already set in the shell. Missing
// file is fine. Keeps secrets (SUPABASE_SERVICE_KEY) out of source / shell
// history; .env is gitignored.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}
