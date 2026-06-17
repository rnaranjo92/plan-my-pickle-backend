// Command api runs the PlanMyPickle backend HTTP server.
package main

import (
	"bufio"
	"log"
	"net/http"
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
	// Real SMS (game-starting alerts) only when Twilio is configured; otherwise
	// the mock records notifications without sending. from = E.164 number or a
	// Messaging Service SID (MG…). Secrets live in the platform env, never code.
	if sid, tok, from := os.Getenv("TWILIO_ACCOUNT_SID"), os.Getenv("TWILIO_AUTH_TOKEN"), os.Getenv("TWILIO_FROM"); sid != "" && tok != "" && from != "" {
		svc.Sms = gateway.NewTwilioSms(sid, tok, from)
		log.Printf("SMS: Twilio configured (from %s)", from)
	} else {
		log.Printf("SMS: mock — set TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_FROM to send real texts")
	}

	handler := api.NewServer(svc)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
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
