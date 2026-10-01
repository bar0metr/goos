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

// SignalState holds what a process told the kernel about signals, its blocked
// mask and a disposition for each signal, and the signals pending for it.
// Standard signals do not queue: a second instance of a pending signal is
// merged with the first, whose information is kept.
type SignalState struct {
	mask    uint64
	pending uint64
	actions [abi.SignalCount][SignalActionSize]byte
	info    [standardSignals]SignalInfo
}

// supportedActionFlags are the sigaction flags whose Linux meaning GoOS
// provides. SA_NOCLDSTOP, SA_ONSTACK, and SA_RESTART hold trivially: no child
// ever stops, no alternate stack can be installed, and no call is interrupted.
const supportedActionFlags = abi.SignalActionNoChildStop | abi.SignalActionNoChildWait | abi.SignalActionInfo |
	abi.SignalActionRestorer | abi.SignalActionOnStack | abi.SignalActionRestart | abi.SignalActionNoDefer |
	abi.SignalActionResetHand

// standardSignals is the count of non-realtime signals, 1 to 31.
const standardSignals = 31

// SignalInfo is the part of siginfo a delivered signal carries: who sent it,
// and for SIGCHLD how the child ended.
type SignalInfo struct {
	Code   int32
	PID    int32
	UID    uint32
	Status int32
}

// SignalAction is one decoded struct sigaction.
type SignalAction struct {
	Handler  uint64
	Flags    uint64
	Restorer uint64
	Mask     uint64
}

// DeliverableSignal reports whether GoOS can generate and deliver signal:
// SIGHUP, SIGINT, SIGQUIT, SIGABRT, SIGKILL, SIGUSR1, SIGUSR2, SIGPIPE,
// SIGALRM, SIGTERM, and SIGCHLD.
// A CPU fault ends the process at once and cannot be caught.
func DeliverableSignal(signal uint64) bool {
	switch signal {
	case abi.SignalHangup, abi.SignalInterrupt, abi.SignalQuit, abi.SignalAbort, abi.SignalKill,
		abi.SignalUser1, abi.SignalUser2, abi.SignalPipe, abi.SignalAlarm, abi.SignalTerm, abi.SignalChild:
		return true
	}
	return false
}

// DefaultIgnored reports whether the default action of a deliverable signal is
// to discard it. Every other deliverable signal ends the process by default.
func DefaultIgnored(signal uint64) bool { return signal == abi.SignalChild }

// CopyFrom gives state the mask and dispositions of source, as fork does.
// Pending signals stay with source.
func (state *SignalState) CopyFrom(source *SignalState) {
	state.mask = source.mask
	state.pending = 0
	copy(state.actions[:], source.actions[:])
	clear(state.info[:])
}

// ActionOf decodes the disposition of signal.
func (state *SignalState) ActionOf(signal uint64) SignalAction {
	if signal == 0 || signal > abi.SignalCount {
		return SignalAction{}
	}
	action := &state.actions[signal-1]
	return SignalAction{
		Handler:  binary.LittleEndian.Uint64(action[0:8]),
		Flags:    binary.LittleEndian.Uint64(action[8:16]),
		Restorer: binary.LittleEndian.Uint64(action[16:24]),
		Mask:     binary.LittleEndian.Uint64(action[signalActionMask:]),
	}
}

func (state *SignalState) ignored(signal uint64) bool {
	handler := state.ActionOf(signal).Handler
	return handler == abi.SignalHandlerIgnore || handler == abi.SignalHandlerDefault && DefaultIgnored(signal)
}

// Raise makes signal pending. An ignored signal that is not blocked is
// discarded, as Linux discards it; SIGKILL is never ignored. It reports
// whether the signal is pending afterwards.
func (state *SignalState) Raise(signal uint64, info SignalInfo) bool {
	if state == nil || !DeliverableSignal(signal) {
		return false
	}
	bit := uint64(1) << (signal - 1)
	if signal != abi.SignalKill && state.mask&bit == 0 && state.ignored(signal) {
		return false
	}
	if state.pending&bit == 0 {
		state.pending |= bit
		state.info[signal-1] = info
	}
	return true
}

// ReapsChildren reports whether children of this process skip the zombie
// state: SIGCHLD is ignored, or its action carries SA_NOCLDWAIT.
func (state *SignalState) ReapsChildren() bool {
	action := state.ActionOf(abi.SignalChild)
	return action.Handler == abi.SignalHandlerIgnore || action.Flags&abi.SignalActionNoChildWait != 0
}

// Fatal reports whether signal sent now would end the process by its default
// action without waiting for the process to run: SIGKILL, or an unblocked
// signal whose default action terminates.
func (state *SignalState) Fatal(signal uint64) bool {
	if signal == abi.SignalKill {
		return true
	}
	return DeliverableSignal(signal) && state.mask&(uint64(1)<<(signal-1)) == 0 && state.Outcome(signal) == SignalTerminate
}

// Pending reports the pending set.
func (state *SignalState) Pending() uint64 { return state.pending }

// Take removes and returns the next signal to deliver: SIGKILL first, then
// the lowest-numbered pending signal the mask does not block.
func (state *SignalState) Take() (uint64, SignalInfo, bool) {
	if state == nil {
		return 0, SignalInfo{}, false
	}
	ready := state.pending &^ (state.mask &^ uncatchable)
	if ready == 0 {
		return 0, SignalInfo{}, false
	}
	signal := uint64(1)
	if ready&(uint64(1)<<(abi.SignalKill-1)) == 0 {
		for ready&(uint64(1)<<(signal-1)) == 0 {
			signal++
		}
	} else {
		signal = abi.SignalKill
	}
	state.pending &^= uint64(1) << (signal - 1)
	info := state.info[signal-1]
	state.info[signal-1] = SignalInfo{}
	return signal, info, true
}

// SignalOutcome is what delivering a taken signal does.
type SignalOutcome uint8

const (
	SignalDiscard SignalOutcome = iota + 1
	SignalTerminate
	SignalHandle
)

// Outcome decides what a taken signal does under its current disposition.
func (state *SignalState) Outcome(signal uint64) SignalOutcome {
	if signal == abi.SignalKill {
		return SignalTerminate
	}
	switch handler := state.ActionOf(signal).Handler; {
	case handler == abi.SignalHandlerIgnore:
		return SignalDiscard
	case handler == abi.SignalHandlerDefault && DefaultIgnored(signal):
		return SignalDiscard
	case handler == abi.SignalHandlerDefault:
		return SignalTerminate
	}
	return SignalHandle
}

// EnterHandler applies what running a handler does to the signal state: the
// handler's mask and, without SA_NODEFER, the signal itself join the blocked
// mask, and SA_RESETHAND restores the default disposition. It returns the
// mask the handler's return restores.
func (state *SignalState) EnterHandler(signal uint64, action SignalAction) uint64 {
	previous := state.mask
	state.mask |= action.Mask
	if action.Flags&abi.SignalActionNoDefer == 0 {
		state.mask |= uint64(1) << (signal - 1)
	}
	state.mask &^= uncatchable
	if action.Flags&abi.SignalActionResetHand != 0 && signal != abi.SignalKill && signal != abi.SignalStop {
		state.actions[signal-1] = [SignalActionSize]byte{}
	}
	return previous
}

// RestoreMask installs the mask rt_sigreturn found in the signal frame.
func (state *SignalState) RestoreMask(mask uint64) {
	state.mask = mask &^ uncatchable
}

// ResetForExec restores caught signals to their default disposition. Ignored
// signals, the blocked mask, and pending signals survive image replacement.
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
	// The kernel's sa_flags is 64 bits wide, and libc stores an int in it:
	// SA_RESETHAND sign-extends into the upper half, which Linux ignores.
	if replacement != nil && uint64(binary.LittleEndian.Uint32(replacement[8:12]))&^supportedActionFlags != 0 {
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
