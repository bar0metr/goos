package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
)

func TestWaitRequestAcceptsChildTargetsAndSupportedOptions(t *testing.T) {
	cases := []struct {
		pid     uint64
		options uint64
		target  int32
		noHang  bool
		errno   abi.Errno
	}{
		{7, 0, 7, false, 0},
		{^uint64(0), abi.WaitNoHang, AnyChild, true, 0},
		{0xffffffff, abi.WaitAll | abi.WaitNoThread, AnyChild, false, 0},
		{0, 0, 0, false, abi.EINVAL},
		{^uint64(1), 0, 0, false, abi.EINVAL},
		{7, 2, 0, false, abi.EINVAL},
		{7, 4, 0, false, abi.EINVAL},
		{7, 8, 0, false, abi.EINVAL},
		{7, 0x80000000, 0, false, abi.EINVAL},
	}
	for _, test := range cases {
		target, noHang, errno := WaitRequest(test.pid, test.options)
		if target != test.target || noHang != test.noHang || errno != test.errno {
			t.Fatalf("wait4(%#x, %#x) = %d %v %d", test.pid, test.options, target, noHang, errno)
		}
	}
}

func TestWaitableChildFindsZombiesOfTheCallerOnly(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(300, 1, 0)
	running, _ := registerChildForTest(301, root.PID)
	exited, _ := registerChildForTest(302, root.PID)
	grandchild, _ := registerChildForTest(303, running.PID)
	if _, any := WaitableChild(root.PID, AnyChild); !any {
		t.Fatal("running children did not count")
	}
	if zombie, any := WaitableChild(root.PID, AnyChild); !any || zombie.State != 0 {
		t.Fatalf("zombie before any exit = %#v", zombie)
	}
	markProcessExited(exited.PID, Termination{Status: 3})
	markProcessExited(grandchild.PID, Termination{Status: 4})
	if zombie, any := WaitableChild(root.PID, AnyChild); !any || zombie.PID != exited.PID || zombie.WaitStatus() != 3<<8 {
		t.Fatalf("any child = %#v", zombie)
	}
	if zombie, any := WaitableChild(root.PID, running.PID); !any || zombie.State != 0 {
		t.Fatalf("running child = %#v", zombie)
	}
	if _, any := WaitableChild(root.PID, grandchild.PID); any {
		t.Fatal("a grandchild matched its grandparent")
	}
	if _, any := WaitableChild(1, AnyChild); any {
		t.Fatal("conceptual PID 1 found a waitable child")
	}
	if _, reaped := ReapChild(root.PID, exited.PID); !reaped {
		t.Fatal("reap failed")
	}
	if _, any := WaitableChild(root.PID, exited.PID); any {
		t.Fatal("a reaped child still matched")
	}
}

func TestChildWaiterIsReleasedOnce(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0xc000, 600, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0xc000)
	state.HoldChildWaiter(9)
	if ReleaseChildWaiter(state.PID+1) != 0 || ReleaseChildWaiter(state.PID) != 9 || ReleaseChildWaiter(state.PID) != 0 {
		t.Fatal("the child waiter was not released exactly once")
	}
}
