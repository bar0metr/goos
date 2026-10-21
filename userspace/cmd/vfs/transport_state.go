package main

import (
	"goos/kernel/ramfs"
	supervisor "goos/userspace/init"
	"sync/atomic"
)

// filesystemAvailability is the only transport state observed across the
// worker/owner boundary. A worker publishes failure, never namespace changes
// or logging. An unanswered accepted request may still access shared memory;
// quarantine therefore lasts until PID 1 attaches a filesystem service behind
// a fence, not just until the next correlation.
type filesystemAvailability struct {
	reason      atomic.Uint32
	quarantined atomic.Bool
	// recoveries is owner-only. Count admissions here as well as notices in
	// PID 1: the old process can exit before its notice reaches PID 1.
	recoveries int
}

func (state *filesystemAvailability) fail(reason ramfs.Status, unanswered bool) {
	if unanswered {
		state.quarantined.Store(true)
	}
	state.reason.CompareAndSwap(0, uint32(reason))
}

func (state *filesystemAvailability) failure() ramfs.Status {
	return ramfs.Status(state.reason.Load())
}

// admit decides whether a new filesystem service may use the file cache, and
// clears the failure state for it when it may. The owner calls it at a
// drained barrier. A quarantined cache may still be written by the service
// that left an exchange unanswered; a new endpoint or correlation does not
// stop that. Only fenced, PID 1's statement that every earlier instance has
// exited as the kernel reported, makes it safe to share again. recovered
// reports that this attach ends a quarantine.
func (state *filesystemAvailability) admit(fenced bool) (allowed, recovered bool) {
	recovered = state.quarantined.Load()
	if recovered {
		if !fenced || state.recoveries >= supervisor.MaxRestarts {
			return false, false
		}
		state.recoveries++
	}
	state.reason.Store(0)
	state.quarantined.Store(false)
	return true, recovered
}

func remainingExchange(start, now, duration uint64) (uint64, bool) {
	if now < start || now-start >= duration {
		return 0, false
	}
	return duration - (now - start), true
}
