//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
)

// Handle waits and timers (ADR 0204). A goroutine that waits for handles
// registers them and parks. One idle thread at a time is the poller: it
// waits on the kernel for its own wake flag, every registered handle,
// and the next timer, and makes the goroutines whose handles became ready
// runnable. A thread that registers a handle or an earlier timer wakes the
// poller, so its wait starts again with the new set. When no thread is
// idle, running threads poll without waiting every few scheduling rounds and
// at preemption visits.

// StatusCanceled is what Wait returns when its Cancel was canceled. It is
// not a kernel status.
const StatusCanceled = kcall.Status(0x100)

var poll struct {
	lock  spin
	regs  [MaxWaiters]registration
	count int32
	// snapshot is what the poller waits for; only the poller uses it.
	snapshot [MaxWaiters]registration
}

// Cancel ends every wait that names c, now and later.
func (c *Cancel) Cancel() {
	pin()
	defer unpin()
	c.lock.lock()
	c.done = true
	var waiters [8]registration
	count := c.count
	copy(waiters[:], c.waiters[:count])
	c.count = 0
	c.lock.unlock()
	for index := 0; index < count; index++ {
		w := waiters[index]
		completeWait(w.index-1, w.gen, w.seq, 0, StatusCanceled)
	}
}

// Canceled reports whether c was canceled.
func (c *Cancel) Canceled() bool {
	c.lock.lock()
	done := c.done
	c.lock.unlock()
	return done
}

// Wait parks the calling goroutine until one of the first count interests
// is ready, the monotonic clock reaches deadline nanoseconds (zero for no
// deadline), or cancel, which may be nil, is canceled. It returns the mask of
// ready interests, as sys.WaitAny does, and observes readiness only: the
// caller then receives or reads. Only the goroutine parks; its thread runs
// other goroutines. Called outside a goroutine, on a scheduler stack, it
// waits on the kernel directly.
func Wait(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, deadline int64, cancel *Cancel) (uint64, kcall.Status) {
	return parkWait(interests, nil, count, deadline, cancel)
}

// waitCursor is Wait over cursor interests, as sys.WaitAnyCursor takes
// them: a log interest is ready once the log holds its cursor's record.
func waitCursor(interests *[kcall.WaitAnyMaxObjects]sys.CursorInterest, count int, deadline int64, cancel *Cancel) (uint64, kcall.Status) {
	return parkWait(nil, interests, count, deadline, cancel)
}

// parkWait is Wait for plain or, when plain is nil, cursor interests.
func parkWait(plain *[kcall.WaitAnyMaxObjects]sys.Interest, cursor *[kcall.WaitAnyMaxObjects]sys.CursorInterest, count int, deadline int64, cancel *Cancel) (uint64, kcall.Status) {
	if count < 0 || count > MaxWaiters {
		return 0, kcall.StatusInvalidArgument
	}
	ensureStarted()
	pin()
	m := currentM()
	g := m.curG
	if g == nil {
		unpin()
		if plain == nil {
			return sys.WaitAnyCursor(cursor, count, ticksUntil(deadline))
		}
		return sys.WaitAny(plain, count, ticksUntil(deadline))
	}
	if cancel != nil && cancel.Canceled() {
		unpin()
		return 0, StatusCanceled
	}
	index := indexOf(g)
	seq := g.wait.begin(g.gen, cancel)
	if !register(index, g.gen, seq, plain, cursor, count) {
		g.wait.state.Store(waitIdle)
		unpin()
		return 0, kcall.StatusResourceExhausted
	}
	if cancel != nil {
		cancel.lock.lock()
		added := !cancel.done && cancel.count < len(cancel.waiters)
		if added {
			cancel.waiters[cancel.count] = registration{index: index + 1, gen: g.gen, seq: seq}
			cancel.count++
		}
		done := cancel.done
		cancel.lock.unlock()
		if done {
			completeWait(index, g.gen, seq, 0, StatusCanceled)
		} else if !added {
			completeWait(index, g.gen, seq, 0, kcall.StatusResourceExhausted)
		}
	}
	if deadline != 0 {
		addTimer(sleepEntry{when: deadline, index: index, gen: g.gen, seq: seq, kind: sleepWaitTimeout})
	}
	if count != 0 {
		nudgePoller()
	}
	switchOut(m, g, reasonPark, 0)
	if cancel != nil {
		cancel.lock.lock()
		for i := 0; i < cancel.count; i++ {
			r := cancel.waiters[i]
			if r.index == index+1 && r.gen == g.gen && r.seq == seq {
				cancel.count--
				cancel.waiters[i] = cancel.waiters[cancel.count]
				cancel.waiters[cancel.count] = registration{}
				break
			}
		}
		cancel.lock.unlock()
	}
	if deadline != 0 {
		sched.timers.lock()
		sched.heap.remove(index, g.gen, seq)
		first, ok := sched.heap.first()
		if !ok {
			first = never
		}
		sched.earliest.Store(first)
		sched.timers.unlock()
	}
	g.wait.cancel = nil
	unpin()
	return g.wait.mask, g.wait.status
}

// register records count interests for the goroutine at index, from plain
// or, when plain is nil, from cursor.
func register(index, gen, seq uint32, plain *[kcall.WaitAnyMaxObjects]sys.Interest, cursor *[kcall.WaitAnyMaxObjects]sys.CursorInterest, count int) bool {
	poll.lock.lock()
	defer poll.lock.unlock()
	if int(poll.count)+count > MaxWaiters {
		return false
	}
	slot := 0
	for bit := 0; bit < count; bit++ {
		for poll.regs[slot].index != 0 {
			slot++
		}
		r := registration{index: index + 1, gen: gen, seq: seq, bit: uint8(bit)}
		if plain != nil {
			r.handle, r.events = uint64(plain[bit].Handle), plain[bit].Events
		} else {
			r.handle, r.events, r.cursor = uint64(cursor[bit].Handle), cursor[bit].Events, cursor[bit].Cursor
		}
		poll.regs[slot] = r
		poll.count++
	}
	return true
}

// unregister drops every interest of the goroutine at index.
func unregister(index uint32) {
	poll.lock.lock()
	for slot := range poll.regs {
		if poll.regs[slot].index == index+1 {
			poll.regs[slot] = registration{}
			poll.count--
		}
	}
	poll.lock.unlock()
}

// completeWait ends the wait seq of the goroutine at index, exactly once,
// with mask and status.
func completeWait(index, gen, seq uint32, mask uint64, status kcall.Status) {
	if index >= MaxGoroutines {
		return
	}
	g := &gs[index]
	if !g.wait.claim(gen, seq, mask, status) {
		return
	}
	unregister(index)
	readyG(g)
}

// pollPending reports whether a goroutine waits for a handle.
func pollPending() bool {
	poll.lock.lock()
	pending := poll.count != 0
	poll.lock.unlock()
	return pending
}

// nudgePoller makes the poller wait again with the current set, or wakes
// an idle thread to become the poller.
func nudgePoller() {
	if poller := sched.poller.Load(); poller != 0 {
		wakeThread(&ms[poller-1])
		return
	}
	wakeIdle()
}

// snapshot copies the registered interests after m's wake flag into
// m.interests and returns how many it copied. The poller always waits in the
// cursor format: the kernel takes a zero cursor for every object but the
// log, so one format covers plain and cursor registrations.
func snapshot(m *M) int {
	poll.lock.lock()
	count := 0
	for slot := range poll.regs {
		r := poll.regs[slot]
		if r.index == 0 {
			continue
		}
		poll.snapshot[count] = r
		m.interests[1+count] = sys.CursorInterest{Handle: handle.ID(r.handle), Events: r.events, Cursor: r.cursor}
		count++
	}
	poll.lock.unlock()
	return count
}

// deliver completes the waits the poller's result names.
func deliver(m *M, mask uint64, status kcall.Status, count int) {
	if status != kcall.StatusOK && status != kcall.StatusTimeout {
		// Separate valid waits may alias the same kernel object. Probe
		// complete original sets, preserving their masks and validation.
		var bits [MaxWaiters]uint8
		seen := uint64(0)
		for slot := 0; slot < count; slot++ {
			if seen&(uint64(1)<<slot) != 0 {
				continue
			}
			r := poll.snapshot[slot]
			members, size := waitGroup(&poll.snapshot, slot, count, &m.interests, &bits)
			seen |= members
			ready, status := sys.WaitAnyCursor(&m.interests, size, 0)
			if status != kcall.StatusTimeout {
				completeWait(r.index-1, r.gen, r.seq, groupReadyMask(ready, &bits, size), status)
			}
		}
		return
	}
	for slot := 0; slot < count; slot++ {
		if mask&(1<<(slot+1)) == 0 {
			continue
		}
		r := poll.snapshot[slot]
		bits := uint64(0)
		for other := slot; other < count; other++ {
			o := poll.snapshot[other]
			if mask&(1<<(other+1)) != 0 && o.index == r.index && o.gen == r.gen && o.seq == r.seq {
				bits |= 1 << o.bit
				mask &^= 1 << (other + 1)
			}
		}
		completeWait(r.index-1, r.gen, r.seq, bits, kcall.StatusOK)
	}
}

// pollNow polls without waiting when no thread is the poller, and reports
// whether it made a goroutine runnable.
func pollNow(m *M) bool {
	if !pollPending() || !sched.poller.CompareAndSwap(0, m.slot+1) {
		return false
	}
	before := sched.wakes.Load()
	m.interests[0] = wakeInterest
	count := snapshot(m)
	mask, status := sys.WaitAnyCursor(&m.interests, 1+count, 0)
	m.counters.polls.Add(1)
	if mask&1 != 0 {
		drainWake(m)
	}
	deliver(m, mask, status, count)
	sched.poller.Store(0)
	return sched.wakes.Load() != before
}

// idleTicks is how long an idle poller waits before it collects the
// garbage allocated since the last collection (ADR 0201).
const idleTicks = clock.TicksPerSecond / 10

// wait blocks an idle thread. The first idle thread becomes the poller;
// the others wait for their wake flag only.
func wait(m *M) {
	if !sched.poller.CompareAndSwap(0, m.slot+1) {
		waitOwn(m, never)
		return
	}
	m.interests[0] = wakeInterest
	count := snapshot(m)
	deadline := nextDeadline()
	suspect := count == 0 && deadline == never && allAsleep()
	if suspect {
		// Look again after a while in which nothing ran: a goroutine made
		// runnable by another thread an instant ago counts as asleep for
		// a moment in between.
		deadline = nanotime() + deadlockSettle
	}
	before := sched.dispatches.Load() + sched.wakes.Load()
	sched.pollUntil.Store(deadline)
	timeout := ticksUntil(deadline)
	if suspect {
		mask, status := sys.WaitAnyCursor(&m.interests, 1, timeout)
		if mask == 0 && sched.dispatches.Load()+sched.wakes.Load() == before && allAsleep() && pollCount() == 0 && nextDeadline() == never {
			deadlock()
		}
		finishPoll(m, mask, status, 0)
		return
	}
	if (timeout == kcall.WaitAnyInfiniteTimeout || timeout > idleTicks) && arena.IdlePending() {
		mask, status := sys.WaitAnyCursor(&m.interests, 1+count, idleTicks)
		if status == kcall.StatusTimeout {
			arena.CollectIdle()
			timeout = ticksUntil(deadline)
			mask, status = sys.WaitAnyCursor(&m.interests, 1+count, timeout)
		}
		finishPoll(m, mask, status, count)
		return
	}
	mask, status := sys.WaitAnyCursor(&m.interests, 1+count, timeout)
	finishPoll(m, mask, status, count)
}

func finishPoll(m *M, mask uint64, status kcall.Status, count int) {
	sched.pollUntil.Store(never)
	m.counters.polls.Add(1)
	if mask&1 != 0 {
		drainWake(m)
	}
	deliver(m, mask, status, count)
	checkTimers()
	if status != kcall.StatusOK && status != kcall.StatusTimeout && count != 0 && pollPending() {
		// The aggregate can be invalid solely because separate Gs watch
		// aliased objects. Their individual probes above were valid. Wait
		// at most one tick before probing again, rather than busy-looping
		// or sleeping forever on a set the kernel refuses to register.
		drainWake(m)
		if !anyWork() {
			m.interests[0] = wakeInterest
			wakeMask, _ := sys.WaitAnyCursor(&m.interests, 1, min(uint64(1), ticksUntil(nextDeadline())))
			if wakeMask&1 != 0 {
				drainWake(m)
			}
			checkTimers()
		}
	}
	// Keep ownership until delivery no longer reads poll.snapshot.
	sched.poller.Store(0)
	// Hand the poller's place on while goroutines still wait.
	if pollPending() || nextDeadline() != never {
		wakeIdle()
	}
}

// deadlockSettle is how long the poller waits, with nothing running,
// before it reports a deadlock it suspects.
const deadlockSettle = 50e6

// pollCount returns how many handle interests are registered.
func pollCount() int32 {
	poll.lock.lock()
	count := poll.count
	poll.lock.unlock()
	return count
}

// allAsleep reports whether every goroutine waits with nothing that could
// wake it: every worker is idle, no timer is being fired, no thread.Start
// goroutine runs, and no thread is inside a system call for a goroutine.
func allAsleep() bool {
	if sched.live.Load() == 0 || sched.firing.Load() != 0 || anyWork() {
		return false
	}
	sched.lock.lock()
	asleep := sched.nidle.Load() == sched.workers
	sched.lock.unlock()
	if !asleep {
		return false
	}
	for index := range gs {
		status := gs[index].status.Load()
		if status == gRunning || status == gWaking || status == gRunnable || status == gRunnableWaking {
			return false
		}
	}
	return true
}

// addTimer adds a sleeping goroutine or a wait deadline.
func addTimer(entry sleepEntry) {
	sched.timers.lock()
	ok := sched.heap.push(entry)
	first, _ := sched.heap.first()
	sched.earliest.Store(first)
	sched.timers.unlock()
	if !ok {
		fatal("goroutine: timer table full")
	}
	noteDeadline(entry.when)
}

// noteDeadline wakes the poller when when comes before its wait ends.
func noteDeadline(when int64) {
	if poller := sched.poller.Load(); poller != 0 {
		if when < sched.pollUntil.Load() {
			wakeThread(&ms[poller-1])
		}
		return
	}
	wakeIdle()
}

// nextDeadline returns the earliest timer.
func nextDeadline() int64 {
	return min(sched.earliest.Load(), sched.runtimeEarliest.Load())
}

// timersDue reports whether a timer is due now.
func timersDue() bool {
	deadline := nextDeadline()
	return deadline != never && nanotime() >= deadline
}

// checkTimers fires every due sleep, wait deadline, and runtime timer.
func checkTimers() {
	deadline := nextDeadline()
	if deadline == never {
		return
	}
	now := nanotime()
	if now < deadline {
		return
	}
	sched.firing.Add(1)
	defer sched.firing.Add(-1)
	for {
		var due [16]sleepEntry
		count := 0
		sched.timers.lock()
		for count < len(due) {
			first, ok := sched.heap.first()
			if !ok || first > now {
				break
			}
			due[count] = sched.heap.pop()
			count++
		}
		first, ok := sched.heap.first()
		if !ok {
			first = never
		}
		sched.earliest.Store(first)
		sched.timers.unlock()
		for index := 0; index < count; index++ {
			fire(due[index])
		}
		if count < len(due) {
			break
		}
	}
	if sched.runtimeEarliest.Load() <= now {
		sched.runtimeEarliest.Store(never)
		runtimeTimerRun(now)
		next := runtimeTimerNext()
		for {
			current := sched.runtimeEarliest.Load()
			if next >= current || sched.runtimeEarliest.CompareAndSwap(current, next) {
				break
			}
		}
	}
}

func fire(entry sleepEntry) {
	g := &gs[entry.index]
	if g.gen != entry.gen {
		return
	}
	switch entry.kind {
	case sleepWake:
		if g.sleep == entry.seq {
			readyG(g)
		}
	case sleepWaitTimeout:
		completeWait(entry.index, entry.gen, entry.seq, 0, kcall.StatusTimeout)
	}
}

// ticksUntil converts a deadline into the kernel's timeout, in ticks,
// rounded up.
func ticksUntil(deadline int64) uint64 {
	if deadline == never || deadline == 0 {
		return kcall.WaitAnyInfiniteTimeout
	}
	now := nanotime()
	if deadline <= now {
		return 0
	}
	perTick := int64(1000000000 / clock.TicksPerSecond)
	return uint64((deadline - now + perTick - 1) / perTick)
}
