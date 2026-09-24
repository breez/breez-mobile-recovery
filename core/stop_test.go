package core

import (
	"testing"
	"time"
)

// A restart waits for lnd, not for the library's Stop, which can hang for
// good after lnd is down: lnd's "Shutdown complete" ends the wait.
func TestStopWithinEndsWhenLndIsDown(t *testing.T) {
	hang := func() { select {} }

	start := time.Now()
	go func() {
		time.Sleep(100 * time.Millisecond)
		noteNodeLine("2026-09-24 17:12:26.348 [INF] LTND: Shutdown complete")
	}()
	if !stopWithin(hang, 20*time.Second) {
		t.Fatal("reported a timeout although lnd was down")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("waited %v after lnd was down", took)
	}

	// Without lnd saying so, the bounded wait still applies.
	if stopWithin(hang, 300*time.Millisecond) {
		t.Fatal("reported a stop although nothing stopped")
	}
	// A Stop that returns is a stop.
	if !stopWithin(func() {}, 20*time.Second) {
		t.Fatal("a Stop that returned was reported as a timeout")
	}
}
