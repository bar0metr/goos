package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
)

func TestSignalTargetAllowsSelfAndDescendantsOnly(t *testing.T) {
	resetCompatibilityProcesses()
	root, _ := installProcessRecord(800, 1, 0)
	child, _ := registerChildForTest(801, root.PID)
	grandchild, _ := registerChildForTest(802, child.PID)
	sibling, _ := registerChildForTest(803, root.PID)
	other, _ := installProcessRecord(804, 1, 0)
	cases := []struct {
		sender, target int32
		errno          abi.Errno
	}{
		{root.PID, root.PID, 0},
		{root.PID, child.PID, 0},
		{root.PID, grandchild.PID, 0},
		{child.PID, grandchild.PID, 0},
		{child.PID, root.PID, abi.EPERM},
		{child.PID, sibling.PID, abi.EPERM},
		{root.PID, other.PID, abi.ESRCH},
		{other.PID, child.PID, abi.ESRCH},
		{root.PID, 30000, abi.ESRCH},
	}
	for _, test := range cases {
		if _, errno := SignalTarget(test.sender, test.target); errno != test.errno {
			t.Fatalf("%d -> %d = errno %d, want %d", test.sender, test.target, errno, test.errno)
		}
	}
	markProcessExited(grandchild.PID, Termination{})
	if record, errno := SignalTarget(root.PID, grandchild.PID); errno != 0 || record.State != ProcessZombie {
		t.Fatalf("zombie target = %#v errno %d", record, errno)
	}
}

func TestActionFlagsAndAutomaticReaping(t *testing.T) {
	var state SignalState
	if state.Action(abi.SignalUser1, actionBytes(0x401000, 0x20|abi.SignalActionRestorer, 0x402000, 0), nil) != abi.EINVAL {
		t.Fatal("an unknown sigaction flag was accepted")
	}
	supported := abi.SignalActionRestorer | abi.SignalActionRestart | abi.SignalActionOnStack | abi.SignalActionInfo |
		abi.SignalActionNoChildStop | abi.SignalActionNoDefer | abi.SignalActionResetHand
	if state.Action(abi.SignalUser1, actionBytes(0x401000, supported|0xffffffff00000000, 0x402000, 0), nil) != 0 || state.ReapsChildren() {
		t.Fatal("supported flags were refused")
	}
	state.Action(abi.SignalChild, actionBytes(0x401000, abi.SignalActionRestorer|abi.SignalActionNoChildWait, 0x402000, 0), nil)
	if !state.ReapsChildren() {
		t.Fatal("SA_NOCLDWAIT did not reap children")
	}
	state.Action(abi.SignalChild, actionBytes(abi.SignalHandlerIgnore, 0, 0, 0), nil)
	if !state.ReapsChildren() {
		t.Fatal("SIG_IGN on SIGCHLD did not reap children")
	}

	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0xe000, 900, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	parent, _ := FindProcess(0xe000)
	child, _ := registerChildForTest(901, parent.PID)
	parent.Signals.Action(abi.SignalChild, actionBytes(abi.SignalHandlerIgnore, 0, 0, 0), nil)
	markProcessExited(child.PID, Termination{Status: 1})
	if _, found := FindProcessRecord(child.PID); found {
		t.Fatal("a child of a process ignoring SIGCHLD became a zombie")
	}
}

func TestFatalSignals(t *testing.T) {
	var state SignalState
	if !state.Fatal(abi.SignalTerm) || !state.Fatal(abi.SignalKill) || state.Fatal(abi.SignalChild) || state.Fatal(abi.SignalSegv) {
		t.Fatal("default fatality is wrong")
	}
	state.Mask(abi.SignalBlock, bit(abi.SignalTerm)|bit(abi.SignalKill), true)
	state.Action(abi.SignalUser1, actionBytes(0x401000, abi.SignalActionRestorer, 0x402000, 0), nil)
	if state.Fatal(abi.SignalTerm) || !state.Fatal(abi.SignalKill) || state.Fatal(abi.SignalUser1) {
		t.Fatal("a blocked or handled signal was fatal")
	}
}
