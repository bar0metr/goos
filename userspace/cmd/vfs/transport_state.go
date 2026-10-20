package main

import (
	"goos/kernel/ramfs"
	"sync/atomic"
)

// filesystemAvailability is the only transport state observed across the
// worker/owner boundary. A worker publishes failure, never namespace changes
// or logging. An unanswered accepted request may still access shared memory;
// quarantine therefore lasts for this VFS instance, not just one correlation.
type filesystemAvailability struct {
	reason      atomic.Uint32
	quarantined atomic.Bool
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

func remainingExchange(start, now, duration uint64) (uint64, bool) {
	if now < start || now-start >= duration {
		return 0, false
	}
	return duration - (now - start), true
}
