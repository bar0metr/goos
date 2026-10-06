//go:build uefi

package main

import (
	"encoding/binary"
	"sync/atomic"

	"goos/internal/timekeeping"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/lock"
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

// futexBuckets spread the futex words over locks, so waits and wakes on
// different words run on several processors at once. A bucket holds at most
// one wait per task slot, indexed by slot, so a wake takes waiters oldest
// slot first as before. futexBucketOf names the bucket a task waits in, plus
// one, so a cancel finds the wait without knowing its word.
const futexBucketCount = 32

type futexBucket struct {
	lock    lock.Spin
	waiters [sched.MaxTasks]futexWaiter
}

var (
	futexBuckets  [futexBucketCount]futexBucket
	futexBucketOf [sched.MaxTasks]atomic.Uint32
)

func init() {
	for index := range futexBuckets {
		futexBuckets[index].lock.SetLevel(lock.LevelFutex)
		futexBuckets[index].lock.SetRank(uint32(index + 1))
	}
}

// futexIndex hashes a word to its bucket.
func futexIndex(root uint64, address uint64) int {
	key := (root ^ address>>2) * 0x9e3779b97f4a7c15
	return int(key >> 59)
}

// lockFutexPair takes the buckets of two words, lower rank first, and
// returns them; second is nil when both words share a bucket.
func lockFutexPair(first int, second int) (*futexBucket, *futexBucket) {
	if first == second {
		futexBuckets[first].lock.Lock()
		return &futexBuckets[first], nil
	}
	low, high := first, second
	if low > high {
		low, high = high, low
	}
	futexBuckets[low].lock.Lock()
	futexBuckets[high].lock.LockAfter(&futexBuckets[low].lock)
	return &futexBuckets[first], &futexBuckets[second]
}

func unlockFutexPair(first *futexBucket, second *futexBucket) {
	if second != nil {
		second.lock.Unlock()
	}
	first.lock.Unlock()
}

// futex serves wait and wake. A wait sleeps until a wake with a matching
// bit set, a signal with a handler, or its timeout: an interval for
// FUTEX_WAIT, a point on the monotonic or realtime clock for
// FUTEX_WAIT_BITSET. A requeue wakes some waiters and moves the rest to a
// second word. The priority-inheritance operations and wake-op are refused
// and counted. The word is read with its bucket locked, so a wake on it
// cannot fall between the read and the sleep.
func (handler *linuxCallHandler) futex(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
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
	address, target := request.Arguments[0], request.Arguments[4]
	if address%4 != 0 {
		return linuxErrno(linuxabi.EINVAL)
	}
	operation := request.Arguments[1] &^ (linuxcompat.FutexPrivateFlag | linuxcompat.FutexClockRealtime)
	requeue := operation == linuxcompat.FutexRequeue || operation == linuxcompat.FutexCmpRequeue
	if requeue && target%4 != 0 {
		return linuxErrno(linuxabi.EFAULT)
	}
	first := futexIndex(handler.root, address)
	second := first
	if requeue {
		second = futexIndex(handler.root, target)
	}
	bucket, targetBucket := lockFutexPair(first, second)
	if targetBucket == nil && requeue {
		targetBucket = bucket
	}
	result := handler.futexLocked(request, address, target, bucket, targetBucket, &local.linuxFutexWord)
	if targetBucket == bucket {
		targetBucket = nil
	}
	unlockFutexPair(bucket, targetBucket)
	return result
}

// futexLocked is futex with the word's bucket, and a requeue's target
// bucket, held.
func (handler *linuxCallHandler) futexLocked(request linuxcompat.Request, address uint64, target uint64, bucket *futexBucket, targetBucket *futexBucket, word *[4]byte) linuxcompat.Result {
	if !vm.CopyIn(handler.root, address, word[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	call, errno := linuxcompat.FutexDecide(request.Arguments[1], binary.LittleEndian.Uint32(word[:]), request.Arguments[2], request.Arguments[3], request.Arguments[5])
	if errno != 0 {
		return linuxErrno(errno)
	}
	switch call.Action {
	case linuxcompat.FutexWakeWaiters:
		return linuxcompat.Result{Value: uint64(wakeFutexLocked(bucket, handler.root, address, call.Bitset, call.Count))}
	case linuxcompat.FutexMoveWaiters:
		if targetBucket == nil || !vm.CopyIn(handler.root, target, word[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		woken := wakeFutexLocked(bucket, handler.root, address, call.Bitset, call.Count)
		return linuxcompat.Result{Value: uint64(woken + requeueFutexLocked(bucket, targetBucket, handler.root, address, target, call.Requeue))}
	case linuxcompat.FutexSleep:
		return handler.futexSleep(request, address, call, bucket)
	}
	return linuxcompat.Refuse(request.Number)
}

// futexSleep registers the caller in bucket, which it holds, and blocks it.
func (handler *linuxCallHandler) futexSleep(request linuxcompat.Request, address uint64, call linuxcompat.FutexCall, bucket *futexBucket) linuxcompat.Result {
	local := thisCPU()
	deadline := uint64(0)
	if timeout := request.Arguments[3]; timeout != 0 {
		if !vm.CopyIn(handler.root, timeout, local.linuxFutexTimeout[:]) {
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
	slot, _, valid := handler.task.Parts()
	if !valid || int(slot) >= len(bucket.waiters) {
		return linuxErrno(linuxabi.EINVAL)
	}
	if errno := handler.vfsBegin(request, nil, nil, 0); errno != 0 {
		return linuxErrno(errno)
	}
	bucket.waiters[slot] = futexWaiter{task: handler.task, root: handler.root, address: address, bitset: call.Bitset}
	futexBucketOf[slot].Store(uint32(bucketIndex(bucket) + 1))
	if !linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxFutexCorrelation) ||
		deadline != 0 && !waitAnyTable.Begin(handler.task, nil, deadline) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		bucket.waiters[slot] = futexWaiter{}
		futexBucketOf[slot].Store(0)
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxErrno(linuxabi.EINVAL)
	}
	return linuxBlocked
}

// bucketIndex returns the index of a bucket in futexBuckets.
func bucketIndex(bucket *futexBucket) int {
	for index := range futexBuckets {
		if &futexBuckets[index] == bucket {
			return index
		}
	}
	return 0
}

// futexTicks converts the timeout in linuxFutexTimeout to timer ticks. An
// interval is FUTEX_WAIT's; an absolute time on the monotonic or realtime
// clock is FUTEX_WAIT_BITSET's. A time already passed is zero ticks.
func (handler *linuxCallHandler) futexTicks(call linuxcompat.FutexCall) (uint64, linuxabi.Errno) {
	local := thisCPU()
	if !call.Absolute {
		return linuxcompat.SleepTicks(&local.linuxFutexTimeout, clock.TicksPerSecond, linuxSleepTicks)
	}
	if _, errno := linuxcompat.SleepTicks(&local.linuxFutexTimeout, clock.TicksPerSecond, linuxSleepTicks); errno != 0 {
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
	end := linuxcompat.SleepNanos(&local.linuxFutexTimeout)
	if end <= now {
		return 0, 0
	}
	return linuxcompat.RemainingTicks(end-now, clock.TicksPerSecond, linuxSleepTicks), 0
}

// wakeFutex wakes up to count tasks asleep on the word at address in root
// whose bit set meets bitset, oldest slot first, and reports how many woke.
// The caller holds no futex bucket.
func wakeFutex(root uint64, address uint64, bitset uint32, count uint32) uint32 {
	bucket := &futexBuckets[futexIndex(root, address)]
	bucket.lock.Lock()
	woken := wakeFutexLocked(bucket, root, address, bitset, count)
	bucket.lock.Unlock()
	return woken
}

// wakeFutexLocked is wakeFutex with the word's bucket held.
func wakeFutexLocked(bucket *futexBucket, root uint64, address uint64, bitset uint32, count uint32) uint32 {
	woken := uint32(0)
	for index := range bucket.waiters {
		if woken >= count {
			break
		}
		waiter := bucket.waiters[index]
		if waiter.task == 0 || waiter.root != root || waiter.address != address || waiter.bitset&bitset == 0 {
			continue
		}
		bucket.waiters[index] = futexWaiter{}
		futexBucketOf[index].Store(0)
		waitAnyTable.Cancel(waiter.task)
		if resumeLinuxSyscall(waiter.task, linuxFutexCorrelation, linuxcompat.WakeMessage) {
			woken++
		}
	}
	return woken
}

// requeueFutexLocked moves up to count tasks asleep on the word at address to
// the word at target, oldest slot first, and reports how many moved. They
// stay asleep until a wake on target. The caller holds both buckets, which
// may be one.
func requeueFutexLocked(bucket *futexBucket, targetBucket *futexBucket, root uint64, address uint64, target uint64, count uint32) uint32 {
	moved := uint32(0)
	destination := uint32(bucketIndex(targetBucket) + 1)
	for index := range bucket.waiters {
		if moved >= count {
			break
		}
		waiter := bucket.waiters[index]
		if waiter.task == 0 || waiter.root != root || waiter.address != address {
			continue
		}
		waiter.address = target
		bucket.waiters[index] = futexWaiter{}
		targetBucket.waiters[index] = waiter
		futexBucketOf[index].Store(destination)
		moved++
	}
	return moved
}

// forgetFutexWaiter removes the wait of task, if it has one. A requeue may
// move the wait while this looks for it, so it follows the wait to the bucket
// it went to.
func forgetFutexWaiter(task sched.TaskID) bool {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(futexBucketOf) {
		return false
	}
	for {
		recorded := futexBucketOf[slot].Load()
		if recorded == 0 {
			return false
		}
		bucket := &futexBuckets[recorded-1]
		bucket.lock.Lock()
		if futexBucketOf[slot].Load() != recorded {
			bucket.lock.Unlock()
			continue
		}
		found := bucket.waiters[slot].task == task
		if found {
			bucket.waiters[slot] = futexWaiter{}
			futexBucketOf[slot].Store(0)
		}
		bucket.lock.Unlock()
		return found
	}
}

// interruptFutexWaiter ends the futex wait of task for a signal.
func interruptFutexWaiter(task sched.TaskID) bool {
	if !forgetFutexWaiter(task) {
		return false
	}
	waitAnyTable.Cancel(task)
	return resumeLinuxSyscall(task, linuxFutexCorrelation, linuxcompat.WakeInterrupted)
}
