package service

import (
	"errors"
	"log"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/store"
)

// jokeJobName is the daily_jobs key for the joke push.
const jokeJobName = "joke_of_the_day"

// SendJokeOfTheDay schedules the daily joke push, at most once per day.
//
// Delivered at 9am in EACH subscriber's own timezone, which OneSignal does
// natively (delayed_option=timezone). That matters: this is a good-morning
// message, and a good-morning message that lands at 2am is a reason to turn
// notifications off — which costs far more than the joke is worth.
//
// One notification to a segment, not a fan-out over external ids: this goes to
// everyone, and enumerating every user to say the same sentence is a lot of
// payload for no gain.
func (s *Service) SendJokeOfTheDay() {
	if !s.claimDailyJob(jokeJobName) {
		return // already sent today, or the marker table isn't there yet
	}
	// The joke for the day this will LAND on, not the day it is queued.
	//
	// OneSignal holds this until 9am in each subscriber's own timezone, but the
	// day is claimed once per UTC day — so the first tick after 00:00 UTC queues
	// it at 5pm Pacific, and it arrives the NEXT morning. Choosing on the queue
	// day meant every push carried the joke the card had already shown all
	// yesterday: two surfaces, same feature, one day apart.
	day := jokeDeliveryDay()
	joke := JokeOfTheDay(day)
	if joke == "" {
		log.Printf("joke push: no joke for %s — nothing sent",
			day.Format("2006-01-02"))
		s.releaseDailyJob(jokeJobName)
		return
	}
	if err := s.sendPushToEveryoneAt9am("Joke of the day 🥒", joke); err != nil {
		// Give the day BACK so the next hourly tick retries.
		//
		// The claim is taken before the send (it has to be — it's what stops two
		// instances sending twice). But every failure exit used to keep it, so a
		// single bad minute burned the whole day: no retry until 00:00 UTC, and
		// the only symptom was nobody getting a joke.
		log.Printf("joke push: FAILED to hand to OneSignal (%v) — releasing the "+
			"day so the next tick retries", err)
		s.releaseDailyJob(jokeJobName)
		return
	}
	// A success line, not just a failure one. This job runs unattended once a
	// day and every one of its exits was previously silent, so "did it go?" had
	// no answer short of waiting for 9am and asking a human.
	log.Printf("joke push: queued for 9am local (claimed %s UTC, delivers %s)",
		time.Now().UTC().Format("2006-01-02"), day.Format("2006-01-02"))
}

// JokeLastRunDay reports the day the joke job last claimed (UTC, "2006-01-02"),
// or "" if it has never run / the table is missing. Exposed on /healthz so
// "did the 9am joke go out?" is answerable with a curl.
func (s *Service) JokeLastRunDay() string {
	if !s.columnReady("daily_jobs", "name") {
		return ""
	}
	row, err := s.sb.SelectOne("daily_jobs",
		"name=eq."+store.Q(jokeJobName)+"&select=ran_on")
	if err != nil || row == nil {
		return ""
	}
	return asStr(row, "ran_on")
}

// jokeDay is the date the joke is chosen for.
//
// A single notification goes to everyone, so ONE day has to be picked — and it
// can't be UTC's, which rolls over at 5pm Pacific. That put the push a day
// ahead of the card for the whole US evening: two surfaces, same feature,
// different joke.
//
// Anchored to Pacific, where the players are. When that changes, this is the
// line to change.
func jokeDay() time.Time {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.Now().UTC() // zoneinfo missing — better a joke than none
	}
	return time.Now().In(loc)
}

// jokeDeliveryDay is the day the 9am push will actually arrive on.
//
// Before 9am Pacific, today's 9am is still ahead, so it lands today. At or
// after 9am it has passed and the notification waits for tomorrow's. The
// scheduled job normally queues at 5pm Pacific, which is the second case.
//
// Separate from jokeDay() on purpose: the IMMEDIATE paths (the manual
// broadcast, the preview) send now and must keep matching the card, which
// shows the caller's own today.
func jokeDeliveryDay() time.Time { return deliveryDayFor(jokeDay()) }

// deliveryDayFor is the pure half, so the rule is testable without waiting for
// 5pm.
func deliveryDayFor(queuedAt time.Time) time.Time {
	if queuedAt.Hour() < 9 {
		return queuedAt
	}
	return queuedAt.AddDate(0, 0, 1)
}

// TodaysJoke is today's joke text, for the owner's manual broadcast.
func (s *Service) TodaysJoke() string {
	return JokeOfTheDay(jokeDay())
}

// ClaimJokeDay marks today's joke as sent, so the scheduled path stands down.
// Used after a manual force-send; best-effort.
func (s *Service) ClaimJokeDay() {
	_ = s.claimDailyJob(jokeJobName)
}

// SendJokePreview sends today's joke to ONE device, immediately.
//
// The real broadcast is scheduled for 9am local and claimed once a day, so
// there is otherwise no way to see the thing you just changed without waiting
// until tomorrow morning.
func (s *Service) SendJokePreview(externalID, subID string) error {
	joke := JokeOfTheDay(jokeDay())
	if joke == "" {
		return errors.New("no jokes are loaded")
	}
	return s.sendTestPushRetrying(externalID, subID, "Joke of the day 🥒", joke)
}

// releaseDailyJob hands a claimed day back, so a later tick can try again.
//
// Rolls ran_on to the epoch rather than to yesterday: the claim test is
// `ran_on < today`, and any date before today satisfies it, so the oldest
// possible value is also the most obviously "not run" to a human reading the
// table.
func (s *Service) releaseDailyJob(name string) {
	if !s.columnReady("daily_jobs", "name") {
		return
	}
	if _, err := s.sb.Update("daily_jobs", "name=eq."+store.Q(name),
		map[string]any{"ran_on": "1970-01-01"}); err != nil {
		log.Printf("daily job %q: could not release the claim: %v", name, err)
	}
}

// claimDailyJob atomically takes today's run of [name], returning false if
// somebody already has it.
//
// The claim is the UPDATE's own WHERE clause rather than a read-then-write:
// two instances booting together would both read "not run today" and both send.
func (s *Service) claimDailyJob(name string) bool {
	if !s.columnReady("daily_jobs", "name") {
		// Silence beats double-sending — but silence with no log meant a
		// missing migration disabled every daily job FOREVER with nothing to
		// find. Say so on each attempt; hourly is rare enough to be signal.
		log.Printf("daily job %q skipped: daily_jobs is not there — run add_daily_jobs.sql", name)
		return false
	}
	today := time.Now().UTC().Format("2006-01-02")
	rows, err := s.sb.Update("daily_jobs",
		"name=eq."+store.Q(name)+"&ran_on=lt."+store.Q(today),
		map[string]any{"ran_on": today})
	if err != nil {
		return false
	}
	if len(rows) > 0 {
		return true // we moved it forward, so the run is ours
	}
	// No row moved: either it already ran today, or the row doesn't exist yet.
	// Insert claims it for a first run; a duplicate-key error means another
	// instance won the race, which is the correct outcome.
	ins, ierr := s.sb.Insert("daily_jobs",
		map[string]any{"name": name, "ran_on": today})
	return ierr == nil && len(ins) > 0
}
