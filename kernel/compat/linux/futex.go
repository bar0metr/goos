package linux

import "goos/kernel/compat/linux/abi"

// Futex operations and the flags that may accompany them.
const (
	FutexWait          = 0
	FutexWake          = 1
	FutexWaitBitset    = 9
	FutexWakeBitset    = 10
	FutexPrivateFlag   = 128
	FutexClockRealtime = 256
	// RobustListHeadSize is the size of struct robust_list_head, the only
	// length set_robust_list accepts.
	RobustListHeadSize = 24
)

// FutexAction is what a futex call comes to in a process with one task.
type FutexAction uint8

const (
	// FutexNoWaiter answers a wake: no other task can be waiting, so it
	// wakes none and returns 0.
	FutexNoWaiter FutexAction = iota + 1
	// FutexChanged answers a wait whose word no longer holds the expected
	// value: EAGAIN, before any sleep.
	FutexChanged
	// FutexRefused is a call GoOS cannot serve: a wait that would sleep,
	// which needs another task to end it, or an operation it lacks.
	FutexRefused
)

// FutexDecide decides a futex call from its operation, the word at its
// address, and the value or bit set it names. The caller has read word;
// an unreadable or misaligned address is EFAULT or EINVAL before this.
func FutexDecide(operation uint64, word uint32, value uint64, bitset uint64) (FutexAction, abi.Errno) {
	switch operation &^ (FutexPrivateFlag | FutexClockRealtime) {
	case FutexWake, FutexWakeBitset:
		if operation&^FutexPrivateFlag == FutexWakeBitset && uint32(bitset) == 0 {
			return 0, abi.EINVAL
		}
		// Linux takes FUTEX_CLOCK_REALTIME only with the waits.
		if operation&FutexClockRealtime != 0 {
			return 0, abi.ENOSYS
		}
		return FutexNoWaiter, 0
	case FutexWait, FutexWaitBitset:
		if operation&^(FutexPrivateFlag|FutexClockRealtime) == FutexWaitBitset && uint32(bitset) == 0 {
			return 0, abi.EINVAL
		}
		if word != uint32(value) {
			return FutexChanged, abi.EAGAIN
		}
		return FutexRefused, 0
	}
	return FutexRefused, 0
}
