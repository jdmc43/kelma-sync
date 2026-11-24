// Package inflight tracks account writes that are still running so manifest
// cursors never move past changes that have been stamped but not committed.
//
// Writers stamp modified_at with a time taken when they start, but readers only
// see a row once it commits. A manifest read in between would hand the client a
// cursor later than that stamp, and the row would never be listed again. The
// manifest therefore uses min(now, start of the oldest running write for the
// account) as its cursor. Tracking is in-process: KelmaSync runs one server
// process per database.
package inflight

import (
	"sync"
	"time"
)

type Tracker struct {
	mu    sync.Mutex
	next  uint64
	users map[string]map[uint64]time.Time
	now   func() time.Time
}

func New(now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{users: map[string]map[uint64]time.Time{}, now: now}
}

// Writes is the process-wide tracker shared by HTTP handlers and workers.
var Writes = New(nil)

// Begin records a write for userID starting now. Call it before the writer takes
// any timestamp it stores; call the returned function after it commits or fails.
func (t *Tracker) Begin(userID string) (done func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.next
	t.next++
	running := t.users[userID]
	if running == nil {
		running = map[uint64]time.Time{}
		t.users[userID] = running
	}
	running[id] = t.now().UTC()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			// The account's map is only dropped once empty, so it is still `running`.
			delete(running, id)
			if len(running) == 0 {
				delete(t.users, userID)
			}
		})
	}
}

// Cursor returns the latest time a manifest for userID may hand out: now, or the
// start of the oldest write still running for that account.
func (t *Tracker) Cursor(userID string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	cursor := t.now().UTC()
	for _, started := range t.users[userID] {
		if started.Before(cursor) {
			cursor = started
		}
	}
	return cursor
}
