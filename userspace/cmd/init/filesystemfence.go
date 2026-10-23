package main

import supervisor "goos/userspace/init"

// filesystemFence is PID 1's record of who may write the file cache. The
// cache is one shared-memory object mapped by the VFS and by the filesystem
// service. When the VFS reports that a service left an exchange unanswered,
// that service may still write the cache, so the VFS refuses to share it
// with another one until PID 1 states a fence: every earlier instance has
// exited, as the kernel reported. The kernel reports the exit once the last
// task of the process has ended, so no code of it can touch the cache again.
type filesystemFence struct {
	// live is the PID of the instance launched last, until its exit is seen.
	live uint64
	// generation names the attachment handed to the VFS last.
	generation uint64
	// handled is the generation of the last replacement notice acted on.
	handled uint64
	// recoveries counts the replacements made for a quarantined cache.
	recoveries int
	// exhausted is set once the bound is reached: no attach is fenced again
	// in this boot, so the VFS keeps a quarantined cache closed.
	exhausted bool
}

// filesystemLostAction is what a replacement notice asks of PID 1.
type filesystemLostAction uint8

const (
	// filesystemLostIgnored: the notice names an attachment that has been
	// replaced already, or one acted on before.
	filesystemLostIgnored filesystemLostAction = iota
	// filesystemLostStop: end the live instance. Its exit is the fence.
	filesystemLostStop
	// filesystemLostAwait: the instance has exited already, and the
	// supervisor starts its replacement behind the fence.
	filesystemLostAwait
	// filesystemLostExhausted: the bound is reached; the cache stays
	// quarantined until the VFS itself is replaced.
	filesystemLostExhausted
)

// filesystemRecoveryLimit bounds the replacements one boot makes for a
// quarantined cache. Each one follows a whole request deadline, longer than
// half the supervisor's restart window, so that window alone never stops a
// service that stalls every time it is started.
const filesystemRecoveryLimit = supervisor.MaxRestarts

// launched records a new instance and reports whether its attach is fenced:
// the instance before it has exited and the bound is not reached. The
// supervisor starts a service again only after its exit, so an unfenced
// attach means an exit PID 1 has not seen.
func (fence *filesystemFence) launched(pid uint64) bool {
	fenced := fence.live == 0 && !fence.exhausted
	fence.live = pid
	return fenced
}

// exited records a process exit the kernel reported.
func (fence *filesystemFence) exited(pid uint64) {
	if pid != 0 && pid == fence.live {
		fence.live = 0
	}
}

// attaching returns the generation of the attachment about to be sent.
func (fence *filesystemFence) attaching() uint64 {
	fence.generation++
	return fence.generation
}

// lost decides what to do about a notice that the service of an attachment
// generation left an exchange unanswered.
func (fence *filesystemFence) lost(generation uint64) filesystemLostAction {
	if generation == 0 || generation != fence.generation || generation == fence.handled || fence.exhausted {
		return filesystemLostIgnored
	}
	fence.handled = generation
	if fence.recoveries >= filesystemRecoveryLimit {
		fence.exhausted = true
		return filesystemLostExhausted
	}
	fence.recoveries++
	if fence.live == 0 {
		return filesystemLostAwait
	}
	return filesystemLostStop
}
