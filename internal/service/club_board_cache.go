package service

import (
	"sync"
	"time"

	"github.com/rnaranjo92/plan-my-pickle-backend/internal/model"
)

// clubBoardCache is a small TTL memo for the all-time club leaderboard, which
// is expensive to compute (per-event, per-bracket standings) and hit anonymously
// on both the leaderboard endpoint and every club-home load.
//
// 60 seconds: the board is all-time and changes only as matches complete, so a
// minute of staleness is imperceptible while a crawler loop is fully absorbed.
// time.Now is used deliberately (this is not resumable workflow code).
type boardCache struct {
	mu  sync.Mutex
	ttl time.Duration
	v   map[string]boardEntry
}

type boardEntry struct {
	at   time.Time
	rows []model.ClubStanding
}

var clubBoardCache = &boardCache{ttl: 60 * time.Second, v: map[string]boardEntry{}}

func (c *boardCache) get(clubID string) ([]model.ClubStanding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.v[clubID]
	if !ok || time.Since(e.at) > c.ttl {
		return nil, false
	}
	return e.rows, true
}

func (c *boardCache) put(clubID string, rows []model.ClubStanding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.v[clubID] = boardEntry{at: time.Now(), rows: rows}
}
