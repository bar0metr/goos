//go:build goos_userspace && goos_goroutines

package goroutine

// Stats are the scheduler's counters (ADR 0204). Every field is read
// without stopping the threads, so the fields of one call may come from
// slightly different moments; each one is a single atomic or locked read.
type Stats struct {
	// Goroutines.
	Live, Peak, Created, Limit uint64
	Runnable, Running, Parked  uint64
	// Threads and permits.
	MaxProcs, Workers, Pinned, Idle, Spinning, FreePermits uint64
	// Queues.
	GlobalQueue, LocalQueues uint64
	// Events.
	Dispatches, Steals, Stolen, Parks, Wakes uint64
	PreemptVisits, Preemptions, Polls        uint64
	Waiters, Timers                          uint64
	// StackReserved is the address space of goroutine and scheduler
	// stacks, guards included; StackPool the free stacks kept.
	StackReserved, StackPool uint64
}

// ReadStats fills s.
func ReadStats(s *Stats) {
	ensureStarted()
	*s = Stats{}
	s.Live = uint64(sched.live.Load())
	s.MaxProcs = uint64(sched.maxProcs.Load())
	s.Spinning = uint64(sched.spinning.Load())
	s.Idle = uint64(sched.nidle.Load())
	s.Wakes = sched.wakes.Load()
	if free := sched.free.Load(); free > 0 {
		s.FreePermits = uint64(free)
	}
	sched.lock.lock()
	s.Peak, s.Created, s.Limit = uint64(sched.peak), sched.created, uint64(sched.limit)
	s.Workers, s.Pinned = uint64(sched.workers), uint64(sched.pinned)
	s.GlobalQueue = uint64(sched.global.length.Load())
	sched.lock.unlock()
	for index := range gs {
		switch gs[index].status.Load() {
		case gRunnable, gRunnableWaking:
			s.Runnable++
		case gRunning, gWaking:
			s.Running++
		case gParked:
			s.Parked++
		}
	}
	for slot := range ms {
		m := &ms[slot]
		if m.kind.Load() == mWorker {
			s.LocalQueues += uint64(m.queue.length())
		}
		c := &m.counters
		s.Dispatches += c.dispatches.Load()
		s.Steals += c.steals.Load()
		s.Stolen += c.stolen.Load()
		s.Parks += c.parks.Load()
		s.PreemptVisits += c.preemptVisits.Load()
		s.Preemptions += c.preemptions.Load()
		s.Polls += c.polls.Load()
	}
	poll.lock.lock()
	s.Waiters = uint64(poll.count)
	poll.lock.unlock()
	sched.timers.lock()
	s.Timers = uint64(sched.heap.count)
	sched.timers.unlock()
	s.StackReserved = stacks.reserved.Load()
	stacks.lock.lock()
	for i := range stacks.batches {
		b := &stacks.batches[i]
		if b.base != 0 {
			s.StackPool += uint64(stacksPerBatch - popcount32(b.used))
		}
	}
	stacks.lock.unlock()
}
