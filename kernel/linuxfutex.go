//go:build uefi

package main

import (
	"encoding/binary"

	"goos/internal/timekeeping"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
)

// linuxFutexCorrelation marks the one wait a task sleeping in futex has.
const linuxFutexCorrelation = 1

// futexWaiter is a task asleep on a futex word. Words are keyed by address
// space and address: a Linux process maps no memory another process shares,
// so a private and a shared futex name the same word the same way.
type futexWaiter struct {
	task    sched.TaskID
	root    uint64
	address uint64
	bitset  uint32
}

// futexWaiters holds at most one wait per task.
var futexWaiters [sched.MaxTasks]futexWaiter

var (
	linuxFutexWord    [4]byte
	linuxFutexTimeout [linuxcompat.TimespecSize]byte
)

// futex serves wait and wake. A wait sleeps until a wake with a matching
// bit set, a signal with a handler, or its timeout: an interval for
// FUTEX_WAIT, a point on the monotonic or realtime clock for
// FUTEX_WAIT_BITSET. Requeue, the priority-inheritance operations, and
// wake-op are refused and counted.
func (handler *linuxCallHandler) futex(request linuxcompat.Request) linuxcompat.Result {
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		forgetFutexWaiter(handler.task)
		return linuxErrno(errno)
	}
	if active {
		forgetFutexWaiter(handler.task)
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Finish(handler.task)
		switch pending.Wake {
		case linuxcompat.WakeMessage:
			return linuxcompat.Result{}
		case linuxcompat.WakeTimeout:
			return linuxErrno(linuxabi.ETIMEDOUT)
		}
		// A futex wait is never restarted after a handler: Linux returns
		// EINTR whatever SA_RESTART says.
		return linuxErrno(linuxabi.EINTR)
	}
	address := request.Arguments[0]
	if address%4 != 0 {
		return linuxErrno(linuxabi.EINVAL)
	}
	if !vm.CopyIn(handler.root, address, linuxFutexWord[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	word := binary.LittleEndian.Uint32(linuxFutexWord[:])
	call, errno := linuxcompat.FutexDecide(request.Arguments[1], word, request.Arguments[2], request.Arguments[5])
	if errno != 0 {
		return linuxErrno(errno)
	}
	switch call.Action {
	case linuxcompat.FutexWakeWaiters:
		return linuxcompat.Result{Value: uint64(wakeFutex(handler.root, address, call.Bitset, call.Count))}
	case linuxcompat.FutexSleep:
		return handler.futexSleep(request, address, call)
	}
	return linuxcompat.Refuse(request.Number)
}

func (handler *linuxCallHandler) futexSleep(request linuxcompat.Request, address uint64, call linuxcompat.FutexCall) linuxcompat.Result {
	deadline := uint64(0)
	if timeout := request.Arguments[3]; timeout != 0 {
		if !vm.CopyIn(handler.root, timeout, linuxFutexTimeout[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		ticks, errno := handler.futexTicks(call)
		if errno != 0 {
			return linuxErrno(errno)
		}
		if ticks == 0 {
			return linuxErrno(linuxabi.ETIMEDOUT)
		}
		deadline = amd64.TimerTicks() + ticks
	}
	slot := -1
	for index := range futexWaiters {
		if futexWaiters[index].task == 0 {
			slot = index
			break
		}
	}
	if slot < 0 {
		return linuxErrno(linuxabi.ENOMEM)
	}
	if errno := handler.vfsBegin(request, nil, nil, 0); errno != 0 {
		return linuxErrno(errno)
	}
	futexWaiters[slot] = futexWaiter{task: handler.task, root: handler.root, address: address, bitset: call.Bitset}
	if !linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxFutexCorrelation) ||
		deadline != 0 && !waitAnyTable.Begin(handler.task, nil, deadline) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		forgetFutexWaiter(handler.task)
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxErrno(linuxabi.EINVAL)
	}
	return linuxBlocked
}

// futexTicks converts the timeout in linuxFutexTimeout to timer ticks. An
// interval is FUTEX_WAIT's; an absolute time on the monotonic or realtime
// clock is FUTEX_WAIT_BITSET's. A time already passed is zero ticks.
func (handler *linuxCallHandler) futexTicks(call linuxcompat.FutexCall) (uint64, linuxabi.Errno) {
	if !call.Absolute {
		return linuxcompat.SleepTicks(&linuxFutexTimeout, clock.TicksPerSecond, linuxSleepTicks)
	}
	if _, errno := linuxcompat.SleepTicks(&linuxFutexTimeout, clock.TicksPerSecond, linuxSleepTicks); errno != 0 {
		return 0, errno
	}
	if !monotonicClock.Calibrated() {
		return 0, linuxabi.EINVAL
	}
	now := nowNanos()
	if call.Realtime {
		value, set := realtimeClock.Now(&monotonicClock, amd64.ReadTSC())
		if !set {
			return 0, linuxabi.EINVAL
		}
		now = uint64(value.Seconds)*uint64(timekeeping.NanosPerSecond) + uint64(value.Nanoseconds)
	}
	end := linuxcompat.SleepNanos(&linuxFutexTimeout)
	if end <= now {
		return 0, 0
	}
	return linuxcompat.RemainingTicks(end-now, clock.TicksPerSecond, linuxSleepTicks), 0
}

// wakeFutex wakes up to count tasks asleep on the word at address in root
// whose bit set meets bitset, oldest slot first, and reports how many woke.
func wakeFutex(root uint64, address uint64, bitset uint32, count uint32) uint32 {
	woken := uint32(0)
	for index := range futexWaiters {
		if woken >= count {
			break
		}
		waiter := futexWaiters[index]
		if waiter.task == 0 || waiter.root != root || waiter.address != address || waiter.bitset&bitset == 0 {
			continue
		}
		futexWaiters[index] = futexWaiter{}
		waitAnyTable.Cancel(waiter.task)
		if resumeLinuxSyscall(waiter.task, linuxFutexCorrelation, linuxcompat.WakeMessage) {
			woken++
		}
	}
	return woken
}

// forgetFutexWaiter removes the wait of task, if it has one.
func forgetFutexWaiter(task sched.TaskID) bool {
	for index := range futexWaiters {
		if futexWaiters[index].task == task {
			futexWaiters[index] = futexWaiter{}
			return true
		}
	}
	return false
}

// interruptFutexWaiter ends the futex wait of task for a signal.
func interruptFutexWaiter(task sched.TaskID) bool {
	if !forgetFutexWaiter(task) {
		return false
	}
	waitAnyTable.Cancel(task)
	return resumeLinuxSyscall(task, linuxFutexCorrelation, linuxcompat.WakeInterrupted)
}
