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
	joke := JokeOfTheDay(jokeDay())
	if joke == "" {
		return
	}
	if err := s.sendPushToEveryoneAt9am("Joke of the day 🥒", joke); err != nil {
		log.Printf("joke push: %v", err)
	}
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

// claimDailyJob atomically takes today's run of [name], returning false if
// somebody already has it.
//
// The claim is the UPDATE's own WHERE clause rather than a read-then-write:
// two instances booting together would both read "not run today" and both send.
func (s *Service) claimDailyJob(name string) bool {
	if !s.columnReady("daily_jobs", "name") {
		return false // run add_daily_jobs.sql — silence beats double-sending
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
