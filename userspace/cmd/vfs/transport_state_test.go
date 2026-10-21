package main

import (
	"goos/kernel/ramfs"
	"testing"
)

func TestFilesystemFailureQuarantinesUnansweredSharedMemory(t *testing.T) {
	state := &filesystemAvailability{}
	if state.failure() != ramfs.StatusOK || state.quarantined.Load() {
		t.Fatal("new transport offline")
	}
	state.fail(ramfs.StatusTimeout, true)
	if state.failure() != ramfs.StatusTimeout || !state.quarantined.Load() {
		t.Fatal("unanswered request did not quarantine cache")
	}
	state.fail(ramfs.StatusPeerDied, false)
	if state.failure() != ramfs.StatusTimeout || !state.quarantined.Load() {
		t.Fatal("later failure cleared quarantine or changed original reason")
	}
}

func TestFilesystemFailureBeforeSendDoesNotQuarantine(t *testing.T) {
	state := &filesystemAvailability{}
	state.fail(ramfs.StatusIOError, false)
	if state.failure() != ramfs.StatusIOError || state.quarantined.Load() {
		t.Fatal("unsent request cannot leave a shared-cache writer")
	}
}

func TestExchangeDeadlineIsNotRenewedByStaleReplies(t *testing.T) {
	for _, test := range []struct {
		now, want uint64
		valid     bool
	}{{10, 6, true}, {12, 4, true}, {15, 1, true}, {16, 0, false}, {17, 0, false}, {9, 0, false}} {
		remaining, valid := remainingExchange(10, test.now, 6)
		if remaining != test.want || valid != test.valid {
			t.Fatalf("now %d: remaining=%d valid=%t", test.now, remaining, valid)
		}
	}
}

func TestQuarantinedCacheAdmitsOnlyFencedAttach(t *testing.T) {
	state := &filesystemAvailability{}
	if allowed, recovered := state.admit(false); !allowed || recovered {
		t.Fatal("an unfenced attach of a healthy cache was refused")
	}
	state.fail(ramfs.StatusTimeout, true)
	if allowed, _ := state.admit(false); allowed || !state.quarantined.Load() || state.failure() != ramfs.StatusTimeout {
		t.Fatal("an unfenced attach lifted the quarantine")
	}
	if allowed, recovered := state.admit(true); !allowed || !recovered {
		t.Fatal("a fenced attach did not end the quarantine")
	}
	if state.quarantined.Load() || state.failure() != ramfs.StatusOK {
		t.Fatal("a fenced attach kept the old failure")
	}
}

func TestFailureWithoutQuarantineAdmitsAnyAttach(t *testing.T) {
	state := &filesystemAvailability{}
	state.fail(ramfs.StatusPeerDied, false)
	if allowed, recovered := state.admit(false); !allowed || recovered || state.failure() != ramfs.StatusOK {
		t.Fatal("an ordinary restart needs no fence")
	}
}
