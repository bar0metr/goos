package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
)

func TestWaitRequestAcceptsTargetsAndSupportedOptions(t *testing.T) {
	cases := []struct {
		pid     uint64
		options uint64
		target  int32
		decoded WaitOptions
		errno   abi.Errno
	}{
		{7, 0, 7, WaitOptions{}, 0},
		{^uint64(0), abi.WaitNoHang, AnyChild, WaitOptions{NoHang: true}, 0},
		{0xffffffff, abi.WaitAll | abi.WaitNoThread, AnyChild, WaitOptions{}, 0},
		{0, abi.WaitUntraced, 0, WaitOptions{Untraced: true}, 0},
		{^uint64(1), abi.WaitContinued, -2, WaitOptions{Continued: true}, 0},
		{7, 4, 0, WaitOptions{}, abi.EINVAL},
		{7, 0x80000000, 0, WaitOptions{}, abi.EINVAL},
		{0x80000000, 0, 0, WaitOptions{}, abi.EINVAL},
	}
	for _, test := range cases {
		target, decoded, errno := WaitRequest(test.pid, test.options)
		if target != test.target || decoded != test.decoded || errno != test.errno {
			t.Fatalf("wait4(%#x, %#x) = %d %+v %d", test.pid, test.options, target, decoded, errno)
		}
	}
}

func TestWaitReportsStopAndContinueOnce(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(310, 1, 0)
	child, _ := registerChildForTest(311, root.PID)
	if _, changed := MarkStopped(child.PID, abi.SignalTermStop); !changed {
		t.Fatal("stop not recorded")
	}
	if _, any := WaitableChild(root.PID, AnyChild, WaitOptions{}); !any {
		t.Fatal("stopped child not matched")
	}
	if found, _ := WaitableChild(root.PID, AnyChild, WaitOptions{}); found.PID != 0 {
		t.Fatal("stop reported without WUNTRACED")
	}
	found, _ := WaitableChild(root.PID, 0, WaitOptions{Untraced: true})
	if found.PID != child.PID || found.WaitStatus() != uint32(abi.SignalTermStop)<<8|0x7f {
		t.Fatalf("stopped child = %#v status %#x", found, found.WaitStatus())
	}
	if _, ok := TakeChildReport(root.PID, child.PID); !ok {
		t.Fatal("report not taken")
	}
	if found, _ := WaitableChild(root.PID, AnyChild, WaitOptions{Untraced: true}); found.PID != 0 {
		t.Fatal("stop reported twice")
	}
	MarkContinued(child.PID)
	found, _ = WaitableChild(root.PID, -root.PGID, WaitOptions{Continued: true})
	if found.PID != child.PID || found.WaitStatus() != 0xffff {
		t.Fatalf("continued child = %#v", found)
	}
}

func TestWaitableChildFindsZombiesOfTheCallerOnly(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(300, 1, 0)
	running, _ := registerChildForTest(301, root.PID)
	exited, _ := registerChildForTest(302, root.PID)
	grandchild, _ := registerChildForTest(303, running.PID)
	if _, any := WaitableChild(root.PID, AnyChild, WaitOptions{}); !any {
		t.Fatal("running children did not count")
	}
	if zombie, any := WaitableChild(root.PID, AnyChild, WaitOptions{}); !any || zombie.State != 0 {
		t.Fatalf("zombie before any exit = %#v", zombie)
	}
	markProcessExited(exited.PID, Termination{Status: 3})
	markProcessExited(grandchild.PID, Termination{Status: 4})
	if zombie, any := WaitableChild(root.PID, AnyChild, WaitOptions{}); !any || zombie.PID != exited.PID || zombie.WaitStatus() != 3<<8 {
		t.Fatalf("any child = %#v", zombie)
	}
	if zombie, any := WaitableChild(root.PID, running.PID, WaitOptions{}); !any || zombie.State != 0 {
		t.Fatalf("running child = %#v", zombie)
	}
	if _, any := WaitableChild(root.PID, grandchild.PID, WaitOptions{}); any {
		t.Fatal("a grandchild matched its grandparent")
	}
	if _, any := WaitableChild(1, AnyChild, WaitOptions{}); any {
		t.Fatal("conceptual PID 1 found a waitable child")
	}
	if _, reaped := ReapChild(root.PID, exited.PID); !reaped {
		t.Fatal("reap failed")
	}
	if _, any := WaitableChild(root.PID, exited.PID, WaitOptions{}); any {
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
