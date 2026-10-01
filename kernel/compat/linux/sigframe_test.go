package linux

import (
	"encoding/binary"
	"testing"

	"goos/kernel/compat/linux/abi"
)

func actionBytes(handler, flags, restorer, mask uint64) *[SignalActionSize]byte {
	var action [SignalActionSize]byte
	binary.LittleEndian.PutUint64(action[0:8], handler)
	binary.LittleEndian.PutUint64(action[8:16], flags)
	binary.LittleEndian.PutUint64(action[16:24], restorer)
	binary.LittleEndian.PutUint64(action[24:32], mask)
	return &action
}

func bit(signal uint64) uint64 { return uint64(1) << (signal - 1) }

func TestRaiseDiscardsIgnoredAndKeepsBlockedSignals(t *testing.T) {
	var state SignalState
	if state.Raise(abi.SignalChild, SignalInfo{}) || state.Raise(abi.SignalSegv, SignalInfo{}) || state.Raise(0, SignalInfo{}) {
		t.Fatal("a default-ignored or undeliverable signal became pending")
	}
	state.Action(abi.SignalPipe, actionBytes(abi.SignalHandlerIgnore, 0, 0, 0), nil)
	if state.Raise(abi.SignalPipe, SignalInfo{}) {
		t.Fatal("an ignored signal became pending")
	}
	state.Mask(abi.SignalBlock, bit(abi.SignalPipe)|bit(abi.SignalTerm), true)
	if !state.Raise(abi.SignalPipe, SignalInfo{}) || !state.Raise(abi.SignalTerm, SignalInfo{PID: 5}) || !state.Raise(abi.SignalTerm, SignalInfo{PID: 6}) {
		t.Fatal("a blocked signal was discarded")
	}
	if _, _, ok := state.Take(); ok {
		t.Fatal("a blocked signal was taken")
	}
	state.Mask(abi.SignalUnblock, bit(abi.SignalTerm), true)
	signal, info, ok := state.Take()
	if !ok || signal != abi.SignalTerm || info.PID != 5 || state.Pending() != bit(abi.SignalPipe) {
		t.Fatalf("take = %d %+v %v, pending %#x", signal, info, ok, state.Pending())
	}
}

func TestSIGKILLIsTakenFirstAndNeverBlocked(t *testing.T) {
	var state SignalState
	state.Mask(abi.SignalSetMask, ^uint64(0), true)
	state.Mask(abi.SignalUnblock, bit(abi.SignalInterrupt), true)
	state.Raise(abi.SignalInterrupt, SignalInfo{})
	state.Raise(abi.SignalKill, SignalInfo{})
	if signal, _, _ := state.Take(); signal != abi.SignalKill || state.Outcome(signal) != SignalTerminate {
		t.Fatalf("first taken = %d", signal)
	}
	if signal, _, _ := state.Take(); signal != abi.SignalInterrupt || state.Outcome(signal) != SignalTerminate {
		t.Fatalf("second taken = %d", signal)
	}
}

func TestHandlerEntryMasksAndResets(t *testing.T) {
	var state SignalState
	state.Action(abi.SignalUser1, actionBytes(0x401000, abi.SignalActionRestorer|abi.SignalActionResetHand, 0x402000, bit(abi.SignalTerm)), nil)
	state.Action(abi.SignalUser2, actionBytes(0x401000, abi.SignalActionRestorer|abi.SignalActionNoDefer, 0x402000, 0), nil)
	if state.Outcome(abi.SignalUser1) != SignalHandle || state.Outcome(abi.SignalChild) != SignalDiscard {
		t.Fatal("outcomes are wrong")
	}
	previous := state.EnterHandler(abi.SignalUser1, state.ActionOf(abi.SignalUser1))
	mask, _ := state.Mask(0, 0, false)
	if previous != 0 || mask != bit(abi.SignalUser1)|bit(abi.SignalTerm) || state.ActionOf(abi.SignalUser1).Handler != 0 {
		t.Fatalf("after SIGUSR1 handler entry: mask %#x action %+v", mask, state.ActionOf(abi.SignalUser1))
	}
	state.RestoreMask(previous | bit(abi.SignalKill))
	state.EnterHandler(abi.SignalUser2, state.ActionOf(abi.SignalUser2))
	if mask, _ := state.Mask(0, 0, false); mask != 0 {
		t.Fatalf("SA_NODEFER handler mask = %#x", mask)
	}
}

func TestForkCopiesDispositionsButNotPendingSignals(t *testing.T) {
	var parent, child SignalState
	parent.Mask(abi.SignalBlock, bit(abi.SignalTerm), true)
	parent.Raise(abi.SignalTerm, SignalInfo{})
	child.CopyFrom(&parent)
	if child.Pending() != 0 || parent.Pending() == 0 {
		t.Fatal("fork copied pending signals")
	}
}

func TestSignalFrameRoundTrip(t *testing.T) {
	base, fpu, ok := SignalFrameAddress(0x7fff0000, 0x1000)
	if !ok || (base+8)%16 != 0 || fpu%64 != 0 || base+SignalFrameSize > fpu || fpu+FPUStateSize > 0x7fff0000-signalRedZone {
		t.Fatalf("frame at %#x, FPU image at %#x", base, fpu)
	}
	if _, _, ok := SignalFrameAddress(0x1100, 0x1000); ok {
		t.Fatal("a frame was placed below the stack")
	}
	var context UserContext
	for index := range context.Registers {
		context.Registers[index] = uint64(index + 1)
	}
	var frame [SignalFrameSize]byte
	EncodeSignalFrame(&frame, abi.SignalChild, SignalInfo{Code: abi.SignalCodeChildExit, PID: 7, UID: 1000, Status: 3}, &context, 0x4000, 0x402000, 0x7ffef000)
	if binary.LittleEndian.Uint64(frame[0:8]) != 0x402000 {
		t.Fatal("restorer is not the return address")
	}
	info := frame[SignalFrameInfo(0):]
	if binary.LittleEndian.Uint32(info[0:4]) != 17 || binary.LittleEndian.Uint32(info[8:12]) != 1 ||
		binary.LittleEndian.Uint32(info[16:20]) != 7 || binary.LittleEndian.Uint32(info[20:24]) != 1000 || binary.LittleEndian.Uint32(info[24:28]) != 3 {
		t.Fatalf("siginfo = % x", info[:28])
	}
	var uc [UContextSize]byte
	copy(uc[:], frame[SignalFrameUContext(0):])
	decoded, mask, fpuAddress := DecodeSignalContext(&uc)
	if decoded != context || mask != 0x4000 || fpuAddress != 0x7ffef000 {
		t.Fatalf("decoded %+v mask %#x fpstate %#x", decoded, mask, fpuAddress)
	}
	// mcontext_t: fpregs follows the 23 gregs.
	if binary.LittleEndian.Uint64(uc[ucontextMcontext+23*8:]) != 0x7ffef000 {
		t.Fatal("fpstate is not where mcontext_t keeps fpregs")
	}
	// mcontext_t gregs: REG_RIP is 16 and REG_CSGSFS 18 from uc_mcontext.
	if binary.LittleEndian.Uint64(uc[ucontextMcontext+16*8:]) != 17 || binary.LittleEndian.Uint16(uc[ucontextMcontext+18*8:]) != 0x33 {
		t.Fatal("register layout differs from mcontext_t")
	}
}

func TestSanitizeFPUStateClearsReservedMXCSRBits(t *testing.T) {
	var image, current [FPUStateSize]byte
	mxcsr := func() uint32 { return binary.LittleEndian.Uint32(image[fpuMXCSROffset:]) }
	binary.LittleEndian.PutUint32(image[fpuMXCSROffset:], 0x1f80|0x6000)
	if SanitizeFPUState(&image, &current); mxcsr() != 0x7f80 {
		t.Fatalf("a supported MXCSR changed to %#x", mxcsr())
	}
	binary.LittleEndian.PutUint32(image[fpuMXCSROffset:], 0xffffffff)
	if SanitizeFPUState(&image, &current); mxcsr() != defaultMXCSRMask {
		t.Fatalf("reserved bits survived the default mask: %#x", mxcsr())
	}
	binary.LittleEndian.PutUint32(current[fpuMXCSRMask:], 0xffff)
	binary.LittleEndian.PutUint32(image[fpuMXCSROffset:], 0xffffffff)
	if SanitizeFPUState(&image, &current); mxcsr() != 0xffff {
		t.Fatalf("the processor's own mask was ignored: %#x", mxcsr())
	}
}
