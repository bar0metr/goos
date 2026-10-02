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
		action    FutexAction
		errno     abi.Errno
	}{
		{"wake", FutexWake, 0, 1, 0, FutexNoWaiter, 0},
		{"private wake", FutexWake | FutexPrivateFlag, 0, 0x7fffffff, 0, FutexNoWaiter, 0},
		{"wake bitset", FutexWakeBitset | FutexPrivateFlag, 0, 1, ^uint64(0), FutexNoWaiter, 0},
		{"wake empty bitset", FutexWakeBitset, 0, 1, 0, 0, abi.EINVAL},
		{"wake realtime", FutexWake | FutexClockRealtime, 0, 1, 0, 0, abi.ENOSYS},
		{"wait changed", FutexWait | FutexPrivateFlag, 2, 1, 0, FutexChanged, abi.EAGAIN},
		{"wait compares 32 bits", FutexWait, 1, 1 | 1<<32, 0, FutexRefused, 0},
		{"wait would sleep", FutexWait, 1, 1, 0, FutexRefused, 0},
		{"wait realtime would sleep", FutexWait | FutexClockRealtime, 1, 1, 0, FutexRefused, 0},
		{"wait bitset realtime changed", FutexWaitBitset | FutexClockRealtime, 3, 1, 1, FutexChanged, abi.EAGAIN},
		{"wait empty bitset", FutexWaitBitset, 1, 1, 0, 0, abi.EINVAL},
		{"requeue", 3, 0, 0, 0, FutexRefused, 0},
	}
	for _, c := range cases {
		action, errno := FutexDecide(c.operation, c.word, c.value, c.bitset)
		if action != c.action || errno != c.errno {
			t.Errorf("%s: got %d %d, want %d %d", c.name, action, errno, c.action, c.errno)
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
