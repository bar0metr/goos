package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/sched"
)

func TestPollRevents(t *testing.T) {
	cases := []struct {
		events uint16
		ready  PollReadiness
		want   uint16
	}{
		{PollIn, PollReadiness{Readable: true, Writable: true}, PollIn},
		{PollIn | PollOut, PollReadiness{Readable: true, Writable: true}, PollIn | PollOut},
		{PollReadNormal, PollReadiness{Readable: true}, PollReadNormal},
		{PollOut, PollReadiness{Readable: true}, 0},
		{0, PollReadiness{Hangup: true}, PollHangup},
		{PollOut, PollReadiness{Error: true}, PollError},
	}
	for _, test := range cases {
		if got := PollRevents(test.events, test.ready); got != test.want {
			t.Fatalf("events %#x ready %+v = %#x, want %#x", test.events, test.ready, got, test.want)
		}
	}
	var wire [2 * PollFDSize]byte
	wire[8], wire[12] = 5, byte(PollIn)
	SetPollRevents(wire[:], 1, PollHangup)
	if fd, events := PollEntry(wire[:], 1); fd != 5 || events != PollIn || wire[14] != byte(PollHangup) {
		t.Fatalf("entry = %d %#x revents %#x", fd, events, wire[14])
	}
	if nanos, finite := PollTimeoutNanos(-5); finite || nanos != 0 {
		t.Fatal("a negative timeout was finite")
	}
	if nanos, finite := PollTimeoutNanos(1500); !finite || nanos != 1500000000 {
		t.Fatalf("1500 ms = %d", nanos)
	}
}

func TestInterruptingSignalsAndRestart(t *testing.T) {
	var state SignalState
	state.Raise(abi.SignalUser1, SignalInfo{})
	if state.Interrupting() {
		t.Fatal("a signal under its fatal default interrupts instead of ending the process")
	}
	state.Take()
	state.Action(abi.SignalUser1, actionBytes(0x401000, abi.SignalActionRestorer, 0x402000, 0), nil)
	state.Action(abi.SignalUser2, actionBytes(0x401000, abi.SignalActionRestorer|abi.SignalActionRestart, 0x402000, 0), nil)
	state.Raise(abi.SignalUser2, SignalInfo{})
	if !state.Interrupting() || !state.RestartsInterrupted() {
		t.Fatal("an SA_RESTART handler did not restart")
	}
	state.Raise(abi.SignalUser1, SignalInfo{})
	if state.RestartsInterrupted() {
		t.Fatal("the next handler lacks SA_RESTART but the call restarted")
	}
	state.Mask(abi.SignalBlock, bit(abi.SignalUser1)|bit(abi.SignalUser2), true)
	if state.Interrupting() || !state.RestartsInterrupted() {
		t.Fatal("blocked signals interrupted a call")
	}
	var empty SignalState
	empty.Raise(abi.SignalChild, SignalInfo{})
	if empty.Interrupting() {
		t.Fatal("a discarded SIGCHLD interrupted a call")
	}
}

func TestContinuationAcceptsInterruptedWake(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(2, 1)
	request := Request{Number: abi.SysWait4}
	if !table.Begin(task, request, nil) || !table.Issue(task, 1, 7) || !table.Block(task, 1) {
		t.Fatal("block failed")
	}
	if table.Resume(task, 7, WakeContext) || !table.Resume(task, 7, WakeInterrupted) {
		t.Fatal("interrupted wake was refused")
	}
	if pending, _ := table.Pending(task); pending.Wake != WakeInterrupted || pending.Phase != ContinuationReady {
		t.Fatalf("pending = %+v", pending)
	}
}
