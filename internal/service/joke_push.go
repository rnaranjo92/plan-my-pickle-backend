package service

import (
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
	joke := JokeOfTheDay(time.Now().UTC())
	if joke == "" {
		return
	}
	if err := s.sendPushToEveryoneAt9am("Joke of the day 🥒", joke); err != nil {
		log.Printf("joke push: %v", err)
	}
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
