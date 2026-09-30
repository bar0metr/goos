package linux

import (
	"encoding/binary"

	"goos/kernel/compat/linux/abi"
)

// SignalActionSize is the size of the kernel's struct sigaction on x86_64:
// the handler, the flags, the restorer, and a 64-bit mask.
const SignalActionSize = 32

// signalActionMask is the offset of sa_mask in that structure.
const signalActionMask = 24

// SignalState records what a process told the kernel about signals: its
// blocked mask and a disposition for each signal. GoOS delivers no signal in
// this release, so the state is kept and reported and nothing acts on it. A
// process that Ctrl-C ends is ended as SIGKILL would end it, whatever it
// registered for SIGINT.
type SignalState struct {
	mask    uint64
	actions [abi.SignalCount][SignalActionSize]byte
}

// ResetForExec restores caught signals to their default disposition. Ignored
// signals and the blocked mask survive image replacement.
func (state *SignalState) ResetForExec() {
	if state == nil {
		return
	}
	for index := range state.actions {
		action := &state.actions[index]
		if binary.LittleEndian.Uint64(action[:8]) != 1 {
			*action = [SignalActionSize]byte{}
		}
	}
}

// uncatchable are the mask bits of SIGKILL and SIGSTOP, which no process can
// block.
const uncatchable = uint64(1)<<(abi.SignalKill-1) | uint64(1)<<(abi.SignalStop-1)

// Action reads and replaces the disposition of one signal, as rt_sigaction
// does. previous receives the old disposition when it is not nil; replacement
// becomes the new one when it is not nil. SIGKILL and SIGSTOP take no
// replacement.
func (state *SignalState) Action(signal uint64, replacement *[SignalActionSize]byte, previous *[SignalActionSize]byte) abi.Errno {
	if signal == 0 || signal > abi.SignalCount {
		return abi.EINVAL
	}
	if replacement != nil && (signal == abi.SignalKill || signal == abi.SignalStop) {
		return abi.EINVAL
	}
	current := &state.actions[signal-1]
	if previous != nil {
		*previous = *current
	}
	if replacement != nil {
		*current = *replacement
		// Linux drops SIGKILL and SIGSTOP from the mask a handler would run
		// with, and reports the mask without them.
		stored := binary.LittleEndian.Uint64(current[signalActionMask:]) &^ uncatchable
		binary.LittleEndian.PutUint64(current[signalActionMask:], stored)
	}
	return 0
}

// Mask reads the blocked mask and, when change is set, applies set to it by
// how, as rt_sigprocmask does. It returns the mask from before the call.
func (state *SignalState) Mask(how uint64, set uint64, change bool) (uint64, abi.Errno) {
	previous := state.mask
	if !change {
		return previous, 0
	}
	switch how {
	case abi.SignalBlock:
		state.mask |= set
	case abi.SignalUnblock:
		state.mask &^= set
	case abi.SignalSetMask:
		state.mask = set
	default:
		return previous, abi.EINVAL
	}
	state.mask &^= uncatchable
	return previous, 0
}

// SignalSetSize is the only sigset size the x86_64 calls accept.
const SignalSetSize = 8

// DecodeSignalSet reads a copied 64-bit signal set.
func DecodeSignalSet(wire *[SignalSetSize]byte) uint64 {
	return binary.LittleEndian.Uint64(wire[:])
}

func EncodeSignalSet(wire *[SignalSetSize]byte, set uint64) {
	binary.LittleEndian.PutUint64(wire[:], set)
}
