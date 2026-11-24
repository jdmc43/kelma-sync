package inflight

import (
	"testing"
	"time"
)

func TestCursorStaysAtOldestRunningWrite(t *testing.T) {
	clock := time.Date(2026, 10, 21, 20, 41, 48, 0, time.UTC)
	tracker := New(func() time.Time { return clock })
	if got := tracker.Cursor("u"); !got.Equal(clock) {
		t.Fatalf("idle cursor=%v", got)
	}
	first := tracker.Begin("u")
	clock = clock.Add(time.Second)
	second := tracker.Begin("u")
	other := tracker.Begin("v")
	clock = clock.Add(time.Second)
	if got := tracker.Cursor("u"); !got.Equal(clock.Add(-2 * time.Second)) {
		t.Fatalf("cursor=%v, want oldest running write", got)
	}
	first()
	first() // idempotent
	if got := tracker.Cursor("u"); !got.Equal(clock.Add(-time.Second)) {
		t.Fatalf("cursor after first done=%v", got)
	}
	second()
	if got := tracker.Cursor("u"); !got.Equal(clock) {
		t.Fatalf("cursor after all done=%v", got)
	}
	if got := tracker.Cursor("v"); !got.Equal(clock.Add(-time.Second)) {
		t.Fatalf("other account cursor=%v", got)
	}
	other()
	if len(tracker.users) != 0 {
		t.Fatalf("tracker leaked accounts: %v", tracker.users)
	}
}
