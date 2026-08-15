package service

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// IndexNow: tell search engines a URL changed, instead of waiting to be crawled.
//
// WHAT THIS DOES NOT DO: it does not submit to Google. Google's Indexing API is
// restricted to job postings and livestream events, and using it for anything
// else violates their terms; the sitemap-ping endpoint was retired in 2023. For
// Google the only levers are an honest sitemap, internal links, and inbound
// links — there is no button, and anything claiming otherwise is either against
// the rules or a lie.
//
// What it DOES: one POST notifies Bing, Yandex, Seznam and Naver at once (they
// share the protocol). Smaller than Google, but free, instant, and the events
// here are time-sensitive — a tournament crawled a week after it's published is
// a tournament crawled after registration closed.
//
// Inert until PMP_INDEXNOW_KEY is set, so this ships dark and costs nothing.

const indexNowEndpoint = "https://api.indexnow.org/indexnow"

var (
	indexNowKey  = strings.TrimSpace(os.Getenv("PMP_INDEXNOW_KEY"))
	indexNowHTTP = &http.Client{Timeout: 10 * time.Second}

	// Submitting the same URL repeatedly is rude and gets a host throttled, and
	// an event can be saved several times in a minute while an organizer edits.
	// Collapse to one submission per URL per hour.
	indexNowMu     sync.Mutex
	indexNowRecent = map[string]time.Time{}
)

// IndexNowKey is the verification key, or "" when unset. The key file must be
// served at https://planmypickle.com/<key>.txt containing exactly the key —
// that's how the receiving engines prove you own the host.
func IndexNowKey() string { return indexNowKey }

// SubmitURLs notifies the IndexNow engines that these URLs changed.
// Best-effort and asynchronous: a search ping must never slow down or fail the
// thing the organizer actually asked for.
func SubmitURLs(urls ...string) {
	if indexNowKey == "" || len(urls) == 0 {
		return
	}
	fresh := make([]string, 0, len(urls))
	cutoff := time.Now().Add(-time.Hour)
	indexNowMu.Lock()
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || indexNowRecent[u].After(cutoff) {
			continue
		}
		indexNowRecent[u] = time.Now()
		fresh = append(fresh, u)
	}
	// Bound the map: without this an instance that runs for months accumulates
	// an entry per URL ever submitted.
	if len(indexNowRecent) > 5000 {
		for k, v := range indexNowRecent {
			if v.Before(cutoff) {
				delete(indexNowRecent, k)
			}
		}
	}
	indexNowMu.Unlock()
	if len(fresh) == 0 {
		return
	}

	go func() {
		body, err := json.Marshal(map[string]any{
			"host":        "planmypickle.com",
			"key":         indexNowKey,
			"keyLocation": "https://planmypickle.com/" + indexNowKey + ".txt",
			"urlList":     fresh,
		})
		if err != nil {
			return
		}
		req, err := http.NewRequest(http.MethodPost, indexNowEndpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("content-type", "application/json; charset=utf-8")
		resp, err := indexNowHTTP.Do(req)
		if err != nil {
			log.Printf("indexnow: submit failed (continuing): %v", err)
			return
		}
		defer resp.Body.Close()
		// 200 accepted, 202 accepted-pending-key-validation. Anything else is
		// worth a line in the log but never worth failing anything over.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			log.Printf("indexnow: %d for %d url(s)", resp.StatusCode, len(fresh))
			return
		}
		log.Printf("indexnow: submitted %d url(s)", len(fresh))
	}()
}

// SubmitEventURLs pings the pages an event appears on: its own page, and the
// city/county hubs that list it. A new event changes those hub pages too, and
// they're the ones that rank for "pickleball tournaments <place>".
func SubmitEventURLs(eventID, city, county, state string) {
	if indexNowKey == "" {
		return
	}
	const base = "https://planmypickle.com"
	urls := []string{base + "/e/" + eventID, base + "/sitemap.xml"}
	st := slugForIndex(state)
	if st != "" {
		if c := slugForIndex(city); c != "" {
			urls = append(urls, base+"/pickleball-tournaments/"+st+"/"+c)
		}
		if c := slugForIndex(county); c != "" {
			urls = append(urls, base+"/pickleball-tournaments/"+st+"/"+c)
		}
	}
	SubmitURLs(urls...)
}

// slugForIndex mirrors the API package's slugify. Duplicated rather than shared
// because the dependency runs the wrong way — api imports service, not back.
func slugForIndex(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := true
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
