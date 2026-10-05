package linux

import "goos/kernel/compat/linux/abi"

// Futex operations and the flags that may accompany them.
const (
	FutexWait          = 0
	FutexWake          = 1
	FutexRequeue       = 3
	FutexCmpRequeue    = 4
	FutexWaitBitset    = 9
	FutexWakeBitset    = 10
	FutexPrivateFlag   = 128
	FutexClockRealtime = 256
	// FutexBitsetAll is the bit set of FUTEX_WAIT and FUTEX_WAKE: every
	// waiter matches every waker.
	FutexBitsetAll = uint32(0xffffffff)
	// RobustListHeadSize is the size of struct robust_list_head, the only
	// length set_robust_list accepts.
	RobustListHeadSize = 24
)

// FutexAction is what a futex call comes to.
type FutexAction uint8

const (
	// FutexWakeWaiters wakes up to Count waiters whose bit set meets Bitset.
	FutexWakeWaiters FutexAction = iota + 1
	// FutexChanged answers a wait whose word no longer holds the expected
	// value: EAGAIN, before any sleep.
	FutexChanged
	// FutexSleep blocks the caller on the word until a wake with a
	// matching bit set, a signal, or the timeout.
	FutexSleep
	// FutexRefused is an operation GoOS lacks: the priority-inheritance
	// locks, wake-op, and wait on several words.
	FutexRefused
	// FutexMoveWaiters wakes up to Count waiters on the word and moves up
	// to Requeue more to a second word, where a wake on that word finds
	// them. A condition variable broadcast hands its waiters to the mutex
	// this way.
	FutexMoveWaiters
)

// FutexCall is a decoded futex call.
type FutexCall struct {
	Action FutexAction
	Bitset uint32
	// Count is how many waiters a wake may wake.
	Count uint32
	// Requeue is how many more waiters a requeue may move.
	Requeue uint32
	// Absolute says the timeout is a point in time on the clock Realtime
	// names (FUTEX_WAIT_BITSET); otherwise it is an interval (FUTEX_WAIT).
	Absolute bool
	Realtime bool
}

// FutexDecide decodes a futex call from its operation, the word at its
// address, the value argument, the second value (a requeue's count), and the
// last argument (a bit set, or the value a compared requeue expects). The
// caller has read word; an unreadable or misaligned address is EFAULT or
// EINVAL before this.
func FutexDecide(operation uint64, word uint32, value uint64, value2 uint64, bitset uint64) (FutexCall, abi.Errno) {
	realtime := operation&FutexClockRealtime != 0
	switch operation &^ (FutexPrivateFlag | FutexClockRealtime) {
	case FutexRequeue, FutexCmpRequeue:
		if realtime {
			return FutexCall{}, abi.ENOSYS
		}
		if int32(uint32(value)) < 0 || int32(uint32(value2)) < 0 {
			return FutexCall{}, abi.EINVAL
		}
		if operation&^FutexPrivateFlag == FutexCmpRequeue && word != uint32(bitset) {
			return FutexCall{Action: FutexChanged}, abi.EAGAIN
		}
		return FutexCall{Action: FutexMoveWaiters, Bitset: FutexBitsetAll, Count: uint32(value), Requeue: uint32(value2)}, 0
	case FutexWake, FutexWakeBitset:
		mask := FutexBitsetAll
		if operation&^(FutexPrivateFlag|FutexClockRealtime) == FutexWakeBitset {
			if mask = uint32(bitset); mask == 0 {
				return FutexCall{}, abi.EINVAL
			}
		}
		// Linux takes FUTEX_CLOCK_REALTIME only with the waits.
		if realtime {
			return FutexCall{}, abi.ENOSYS
		}
		count := uint32(value)
		if int32(count) < 0 {
			count = 1<<31 - 1
		}
		return FutexCall{Action: FutexWakeWaiters, Bitset: mask, Count: count}, 0
	case FutexWait, FutexWaitBitset:
		call := FutexCall{Action: FutexSleep, Bitset: FutexBitsetAll, Realtime: realtime}
		if operation&^(FutexPrivateFlag|FutexClockRealtime) == FutexWaitBitset {
			if call.Bitset = uint32(bitset); call.Bitset == 0 {
				return FutexCall{}, abi.EINVAL
			}
			call.Absolute = true
		}
		if word != uint32(value) {
			return FutexCall{Action: FutexChanged}, abi.EAGAIN
		}
		return call, 0
	}
	return FutexCall{Action: FutexRefused}, 0
}
