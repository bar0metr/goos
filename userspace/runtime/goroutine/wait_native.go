//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/tls"
)

var (
	nativeWaitHandler        sys.WaitHandler        = schedulerWait
	nativeReceiveHandler     sys.ReceiveHandler     = schedulerReceive
	nativeProcessWaitHandler sys.ProcessWaitHandler = schedulerProcessWait
	nativeThreadJoinHandler  sys.ThreadJoinHandler  = schedulerThreadJoin
	nativeCursorWaitHandler  sys.CursorWaitHandler  = schedulerCursorWait
	nativeIRQWaitHandler     sys.IRQWaitHandler     = schedulerIRQWait
)

func installWaitHooks() {
	sys.InstallWaitHandler(&nativeWaitHandler)
	sys.InstallReceiveHandler(&nativeReceiveHandler)
	sys.InstallProcessWaitHandler(&nativeProcessWaitHandler)
	sys.InstallThreadJoinHandler(&nativeThreadJoinHandler)
	sys.InstallCursorWaitHandler(&nativeCursorWaitHandler)
	sys.InstallIRQWaitHandler(&nativeIRQWaitHandler)
}

func canParkNativeWait() bool {
	pin()
	defer unpin()
	m := currentM()
	if m == nil || m.curG == nil || arena.InRegion() {
		return false
	}
	if tls.Current().Pins != 1 {
		fatal("goroutine: blocking native wait while pinned")
	}
	return true
}

// schedulerWait runs before sys.WaitAny takes its M-local scratch pin. The
// poller uses kernel waits on scheduler stacks and zero-time polls directly.
func schedulerWait(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, timeout uint64) (uint64, kcall.Status, bool) {
	if count > MaxWaiters {
		return 0, kcall.StatusOK, false
	}
	for index := 0; index < count; index++ {
		if uint64(interests[index].Handle) == kcall.WaitAnyThread {
			return 0, kcall.StatusOK, false
		}
	}
	if !canParkNativeWait() {
		return 0, kcall.StatusOK, false
	}
	// Probe with the kernel before registering: it validates capabilities
	// and event masks, and reports immediately ready interests unchanged.
	mask, status := sys.WaitAny(interests, count, 0)
	if status != kcall.StatusTimeout {
		return mask, status, true
	}
	if count == 0 && timeout == kcall.WaitAnyInfiniteTimeout {
		return 0, kcall.StatusInvalidArgument, true
	}
	deadline, valid := deadlineFromTicks(nanotime(), timeout)
	if !valid {
		return 0, kcall.StatusInvalidArgument, true
	}
	mask, status = Wait(interests, count, deadline, nil)
	return mask, status, true
}

// schedulerCursorWait is schedulerWait for cursor interests: the poller's
// registration keeps each log interest's cursor.
func schedulerCursorWait(interests *[kcall.WaitAnyMaxObjects]sys.CursorInterest, count int, timeout uint64) (uint64, kcall.Status, bool) {
	if count > MaxWaiters {
		return 0, kcall.StatusOK, false
	}
	for index := 0; index < count; index++ {
		if uint64(interests[index].Handle) == kcall.WaitAnyThread {
			return 0, kcall.StatusOK, false
		}
	}
	if !canParkNativeWait() {
		return 0, kcall.StatusOK, false
	}
	mask, status := sys.WaitAnyCursor(interests, count, 0)
	if status != kcall.StatusTimeout {
		return mask, status, true
	}
	if count == 0 && timeout == kcall.WaitAnyInfiniteTimeout {
		return 0, kcall.StatusInvalidArgument, true
	}
	deadline, valid := deadlineFromTicks(nanotime(), timeout)
	if !valid {
		return 0, kcall.StatusInvalidArgument, true
	}
	mask, status = waitCursor(interests, count, deadline, nil)
	return mask, status, true
}

// schedulerProcessWait has the same scheduler-stack, nil-M, region and pin
// policy as the existing native adapters. No sys scratch is pinned here.
func schedulerProcessWait(process handle.ID, timeout uint64) (uint64, kcall.Status, bool) {
	if !canParkNativeWait() {
		return 0, kcall.StatusOK, false
	}
	code, status := waitForProcess(process, timeout, processWaitOps{
		probe: sys.ProcessWait,
		now:   nanotime,
		wait:  waitReady,
	})
	return code, status, true
}

// schedulerThreadJoin parks a goroutine joiner on the thread's end under the
// same fallback policy. Its reservation belongs to the G, not its M.
func schedulerThreadJoin(thread uint64) (uint64, kcall.Status, bool) {
	if !canParkNativeWait() {
		return 0, kcall.StatusOK, false
	}
	code, status := joinThread(thread, threadJoinOps{
		claim: sys.ThreadJoinClaim, collect: sys.ThreadJoinCollect,
		release: sys.ThreadJoinRelease, wait: waitReady,
	})
	return code, status, true
}

// schedulerIRQWait parks a goroutine on an interrupt line's readiness under
// the same fallback policy, then consumes the event with IRQTake.
func schedulerIRQWait(irq handle.ID) (kcall.Status, bool) {
	if !canParkNativeWait() {
		return kcall.StatusOK, false
	}
	return waitForIRQ(irq, irqWaitOps{probe: sys.IRQPoll, take: sys.IRQTake, wait: waitReady}), true
}

// waitReady parks on readiness that the caller then consumes or collects.
func waitReady(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, deadline int64) (uint64, kcall.Status) {
	// Validate against the kernel before registration; readiness itself does
	// not consume the process exit. The poller rechecks after registration.
	mask, status := sys.WaitAny(interests, count, 0)
	if status != kcall.StatusTimeout {
		return mask, status
	}
	return Wait(interests, count, deadline, nil)
}

// schedulerReceive keeps one absolute deadline across readiness races. It
// consumes with IPCReceive; only readiness waiting parks, without retaining
// sys's per-M descriptor or an M-local pointer across the switch.
func schedulerReceive(channel handle.ID, buffer []byte, timeout uint64) (kcall.Status, bool) {
	if !canParkNativeWait() {
		return kcall.StatusOK, false
	}
	status := sys.IPCReceive(channel, buffer)
	if status != kcall.StatusNotFound {
		return status, true
	}
	// IPCWaitReceive has a finite tick timeout, unlike WaitAny's sentinel.
	deadline, valid := deadlineFromTicks(nanotime(), timeout)
	if !valid || timeout == kcall.WaitAnyInfiniteTimeout {
		return kcall.StatusResourceExhausted, true
	}
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: channel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
	for {
		// Validate the native capability/event contract before registering.
		_, status = sys.WaitAny(&interests, 1, 0)
		if status != kcall.StatusOK && status != kcall.StatusTimeout {
			return status, true
		}
		if _, status = Wait(&interests, 1, deadline, nil); status != kcall.StatusOK {
			return status, true
		}
		status = sys.IPCReceive(channel, buffer)
		if status != kcall.StatusNotFound {
			return status, true
		}
		if nanotime() >= deadline {
			return kcall.StatusTimeout, true
		}
	}
}
