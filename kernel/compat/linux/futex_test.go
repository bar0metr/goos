package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestFutexDecide(t *testing.T) {
	cases := []struct {
		name      string
		operation uint64
		word      uint32
		value     uint64
		bitset    uint64
		call      FutexCall
		errno     abi.Errno
	}{
		{"wake", FutexWake, 0, 1, 0, FutexCall{Action: FutexWakeWaiters, Bitset: FutexBitsetAll, Count: 1}, 0},
		{"private wake all", FutexWake | FutexPrivateFlag, 0, 0x7fffffff, 0, FutexCall{Action: FutexWakeWaiters, Bitset: FutexBitsetAll, Count: 0x7fffffff}, 0},
		{"wake negative count", FutexWake, 0, 0xffffffff, 0, FutexCall{Action: FutexWakeWaiters, Bitset: FutexBitsetAll, Count: 1<<31 - 1}, 0},
		{"wake bitset", FutexWakeBitset | FutexPrivateFlag, 0, 1, 4, FutexCall{Action: FutexWakeWaiters, Bitset: 4, Count: 1}, 0},
		{"wake empty bitset", FutexWakeBitset, 0, 1, 0, FutexCall{}, abi.EINVAL},
		{"wake realtime", FutexWake | FutexClockRealtime, 0, 1, 0, FutexCall{}, abi.ENOSYS},
		{"wait changed", FutexWait | FutexPrivateFlag, 2, 1, 0, FutexCall{Action: FutexChanged}, abi.EAGAIN},
		{"wait compares 32 bits", FutexWait, 1, 1 | 1<<32, 0, FutexCall{Action: FutexSleep, Bitset: FutexBitsetAll}, 0},
		{"wait realtime", FutexWait | FutexClockRealtime, 1, 1, 0, FutexCall{Action: FutexSleep, Bitset: FutexBitsetAll, Realtime: true}, 0},
		{"wait bitset", FutexWaitBitset | FutexPrivateFlag, 1, 1, 6, FutexCall{Action: FutexSleep, Bitset: 6, Absolute: true}, 0},
		{"wait bitset changed", FutexWaitBitset | FutexClockRealtime, 3, 1, 1, FutexCall{Action: FutexChanged}, abi.EAGAIN},
		{"wait empty bitset", FutexWaitBitset, 1, 1, 0, FutexCall{}, abi.EINVAL},
		{"requeue", FutexRequeue | FutexPrivateFlag, 5, 1, 0, FutexCall{Action: FutexMoveWaiters, Bitset: FutexBitsetAll, Count: 1}, 0},
		{"compared requeue", FutexCmpRequeue, 5, 0, 5, FutexCall{Action: FutexMoveWaiters, Bitset: FutexBitsetAll}, 0},
		{"compared requeue changed", FutexCmpRequeue | FutexPrivateFlag, 6, 0, 5, FutexCall{Action: FutexChanged}, abi.EAGAIN},
		{"requeue negative", FutexRequeue, 0, 0xffffffff, 0, FutexCall{}, abi.EINVAL},
		{"wake-op", 5, 0, 0, 0, FutexCall{Action: FutexRefused}, 0},
	}
	for _, c := range cases {
		call, errno := FutexDecide(c.operation, c.word, c.value, 0, c.bitset)
		if call != c.call || errno != c.errno {
			t.Errorf("%s: got %+v %d, want %+v %d", c.name, call, errno, c.call, c.errno)
		}
	}
}

func TestUmaskKeepsPermissionBits(t *testing.T) {
	var state ProcessState
	state.umask = DefaultUmask
	if previous := state.SetUmask(0o7777); previous != 0o022 {
		t.Fatalf("previous %o", previous)
	}
	if previous := state.SetUmask(0); previous != 0o777 {
		t.Fatalf("mask kept %o, want the permission bits only", previous)
	}
}
