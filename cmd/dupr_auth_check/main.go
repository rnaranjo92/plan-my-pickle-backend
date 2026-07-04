// dupr_auth_check verifies DUPR partner credentials with a bare token exchange —
// no data is read or written, and no secret or token is ever printed.
//
// Usage (put the creds in your shell env, never in code or chat):
//
//	DUPR_CLIENT_KEY=... DUPR_CLIENT_SECRET=... \
//	DUPR_BASE_URL=https://api.dupr.com/api \
//	go run ./cmd/dupr_auth_check
//
// Leave DUPR_BASE_URL unset to test against UAT (https://uat.mydupr.com/api).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	ck := strings.TrimSpace(os.Getenv("DUPR_CLIENT_KEY"))
	cs := strings.TrimSpace(os.Getenv("DUPR_CLIENT_SECRET"))
	if ck == "" || cs == "" {
		fmt.Println("set DUPR_CLIENT_KEY and DUPR_CLIENT_SECRET in the environment")
		os.Exit(2)
	}
	base := strings.TrimSpace(os.Getenv("DUPR_BASE_URL"))
	if base == "" {
		base = "https://uat.mydupr.com/api"
	}
	base = strings.TrimRight(base, "/")
	version := strings.TrimSpace(os.Getenv("DUPR_API_VERSION"))
	if version == "" {
		version = "v1.0"
	}

	fmt.Printf("token exchange against %s (version %s)…\n", base, version)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/auth/%s/token", base, version), nil)
	if err != nil {
		fmt.Println("request build failed:", err)
		os.Exit(1)
	}
	req.Header.Set("x-authorization",
		base64.StdEncoding.EncodeToString([]byte(ck+":"+cs)))
	start := time.Now()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fmt.Println("network error:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ms := time.Since(start).Milliseconds()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Printf("❌ FAILED — http %d in %dms\n", resp.StatusCode, ms)
		// DUPR error bodies carry a message, not secrets — safe to show trimmed.
		s := strings.TrimSpace(string(raw))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		fmt.Println(s)
		os.Exit(1)
	}
	var env struct {
		Result struct {
			Token  string `json:"token"`
			Expiry string `json:"expiry"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Result.Token == "" {
		fmt.Printf("⚠️  http %d in %dms but no token in the response — unexpected shape\n",
			resp.StatusCode, ms)
		os.Exit(1)
	}
	fmt.Printf("✅ CREDENTIALS GOOD — token issued in %dms (length %d, expiry %s)\n",
		ms, len(env.Result.Token), env.Result.Expiry)
}
