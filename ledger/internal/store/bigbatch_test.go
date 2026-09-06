package store

import (
	"fmt"
	"testing"
	"time"
)

// TestLargeBatchDoesNotDeadlock pins the failure that took a scale test from
// four minutes to not finishing: request bytes in and object bytes out both
// exceed a pipe buffer, so a write-all-then-read-all exchange wedges.
func TestLargeBatchDoesNotDeadlock(t *testing.T) {
	s, _ := buildFixture(t, "linear", 3000)
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		evs, _, _, err := s.eventsDAG("refs/ledger/t", "t")
		if err != nil || len(evs) == 0 {
			t.Error(fmt.Sprintf("read failed: %v (%d events)", err, len(evs)))
		}
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		t.Logf("6000-spec batch completed in %v", d)
	case <-time.After(90 * time.Second):
		t.Fatal("a 6000-spec batch did not complete in 90s - the pipe is wedged")
	}
}
