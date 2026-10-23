package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	supervisor "goos/userspace/init"
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

// A dead writer may trigger its ordinary restart before PID 1 handles its
// lost notice. Count recoveries at admission too, so this event ordering
// cannot bypass the supervisor's bound.
func TestFencedCacheRecoveryStopsAtSupervisorBound(t *testing.T) {
	state := &filesystemAvailability{}
	for recovery := 0; recovery < supervisor.MaxRestarts; recovery++ {
		state.fail(ramfs.StatusTimeout, true)
		if allowed, _ := state.admit(false); allowed || state.recoveries != recovery {
			t.Fatal("an unfenced attempt changed the recovery budget")
		}
		if allowed, recovered := state.admit(true); !allowed || !recovered {
			t.Fatalf("recovery %d refused", recovery)
		}
	}
	state.fail(ramfs.StatusTimeout, true)
	for attempt := 0; attempt < 4; attempt++ {
		if allowed, _ := state.admit(true); allowed || !state.quarantined.Load() || state.failure() != ramfs.StatusTimeout {
			t.Fatal("a fenced attach bypassed the recovery bound")
		}
	}
}

func TestControlHandoffDiscardsUnreadAttachmentsWithoutReplay(t *testing.T) {
	add, _ := vfs.AddSessionRequest(51, 2)
	attach, _ := vfs.SetFilesystemRequest(vfs.FilesystemAttach{Client: 52, Generation: 1}, 3)
	sync, _ := vfs.SyncRequest(4)
	queue := []ipc.Message{add, attach, sync}
	var closed []handle.ID
	current := retireControl(10, 20, func(endpoint handle.ID) (ipc.Message, ipc.Status) {
		if endpoint != 10 {
			t.Fatal("drained new endpoint")
		}
		if len(queue) == 0 {
			return ipc.Message{}, ipc.StatusNotFound
		}
		message := queue[0]
		queue = queue[1:]
		return message, ipc.StatusOK
	}, func(id handle.ID) { closed = append(closed, id) })
	if current != 20 || len(queue) != 0 || len(closed) != 3 || closed[0] != 51 || closed[1] != 52 || closed[2] != 10 {
		t.Fatalf("handoff current=%d closed=%v unread=%d", current, closed, len(queue))
	}
}
