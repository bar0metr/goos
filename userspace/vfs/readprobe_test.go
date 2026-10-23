//go:build goos_vfs_probe && goos_goroutines

package vfsservice

import (
	"sync"
	"testing"
	"time"
)

func TestValidationLocalGateRejectsSequentialReads(t *testing.T) {
	state := validationReadState{localRelease: make(chan struct{})}
	gate := validationReadGate{state: &state, kind: ValidationReadLocal}
	gate.holdWithin(time.Millisecond)
	gate.holdWithin(time.Millisecond)
	if state.localOverlap() {
		t.Fatal("sequential reads counted as overlapping")
	}
	if state.localEntered.Load() != 2 || state.localPeak.Load() != 1 || state.localActive.Load() != 0 || !state.localTimed.Load() {
		t.Fatalf("entered=%d peak=%d active=%d timed=%v", state.localEntered.Load(), state.localPeak.Load(), state.localActive.Load(), state.localTimed.Load())
	}
}

func TestValidationLocalGateRequiresTwoActiveWorkers(t *testing.T) {
	state := validationReadState{localRelease: make(chan struct{})}
	gate := validationReadGate{state: &state, kind: ValidationReadLocal}
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			gate.holdWithin(time.Second)
		}()
	}
	workers.Wait()
	if !state.localOverlap() || state.localActive.Load() != 0 {
		t.Fatalf("overlap=%v active=%d peak=%d timed=%v", state.localOverlap(), state.localActive.Load(), state.localPeak.Load(), state.localTimed.Load())
	}
}
