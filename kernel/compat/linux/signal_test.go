package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestSignalActionsAreRecordedAndReadBack(t *testing.T) {
	var state SignalState
	var handler, previous [SignalActionSize]byte
	handler[0], handler[31] = 0x11, 0x22
	if errno := state.Action(2, &handler, &previous); errno != 0 || previous != [SignalActionSize]byte{} {
		t.Fatalf("first install: errno=%d previous=%v", errno, previous)
	}
	if errno := state.Action(2, nil, &previous); errno != 0 || previous != handler {
		t.Fatalf("read back: errno=%d previous=%v", errno, previous)
	}
	if errno := state.Action(3, nil, &previous); errno != 0 || previous != [SignalActionSize]byte{} {
		t.Fatalf("another signal shares the disposition: %v", previous)
	}
	if errno := state.Action(64, &handler, nil); errno != 0 {
		t.Fatalf("the last signal: errno=%d", errno)
	}
	for _, signal := range []uint64{0, 65, ^uint64(0)} {
		if errno := state.Action(signal, nil, &previous); errno != abi.EINVAL {
			t.Errorf("signal %d: errno=%d", signal, errno)
		}
	}
	// A handler's mask never holds SIGKILL or SIGSTOP.
	var masking [SignalActionSize]byte
	for index := signalActionMask; index < SignalActionSize; index++ {
		masking[index] = 0xff
	}
	if errno := state.Action(10, &masking, nil); errno != 0 {
		t.Fatalf("install with a full mask: errno=%d", errno)
	}
	state.Action(10, nil, &previous)
	var stored [SignalSetSize]byte
	copy(stored[:], previous[signalActionMask:])
	if DecodeSignalSet(&stored) != ^uncatchable {
		t.Fatalf("stored mask = %#x", DecodeSignalSet(&stored))
	}
	// SIGKILL and SIGSTOP can be read and never replaced.
	for _, signal := range []uint64{abi.SignalKill, abi.SignalStop} {
		if errno := state.Action(signal, &handler, nil); errno != abi.EINVAL {
			t.Errorf("signal %d took a disposition", signal)
		}
		if errno := state.Action(signal, nil, &previous); errno != 0 || previous != [SignalActionSize]byte{} {
			t.Errorf("signal %d: errno=%d previous=%v", signal, errno, previous)
		}
	}
}

func TestSignalMask(t *testing.T) {
	var state SignalState
	usr1, kill, stop := uint64(1)<<9, uint64(1)<<8, uint64(1)<<18
	if previous, errno := state.Mask(abi.SignalBlock, usr1|kill|stop, true); errno != 0 || previous != 0 {
		t.Fatalf("block: previous=%#x errno=%d", previous, errno)
	}
	// SIGKILL and SIGSTOP never enter the mask.
	if previous, _ := state.Mask(0, 0, false); previous != usr1 {
		t.Fatalf("mask = %#x, want %#x", previous, usr1)
	}
	if previous, errno := state.Mask(abi.SignalSetMask, 0b110, true); errno != 0 || previous != usr1 {
		t.Fatalf("set: previous=%#x errno=%d", previous, errno)
	}
	if previous, errno := state.Mask(abi.SignalUnblock, 0b010, true); errno != 0 || previous != 0b110 {
		t.Fatalf("unblock: previous=%#x errno=%d", previous, errno)
	}
	if previous, errno := state.Mask(3, 0xff, true); errno != abi.EINVAL || previous != 0b100 {
		t.Fatalf("bad operation: previous=%#x errno=%d", previous, errno)
	}
	// A call with no new set ignores the operation.
	if previous, errno := state.Mask(99, 0xff, false); errno != 0 || previous != 0b100 {
		t.Fatalf("read only: previous=%#x errno=%d", previous, errno)
	}
	var wire [SignalSetSize]byte
	EncodeSignalSet(&wire, 0x8000000000000001)
	if DecodeSignalSet(&wire) != 0x8000000000000001 {
		t.Fatal("signal set does not round-trip")
	}
}
