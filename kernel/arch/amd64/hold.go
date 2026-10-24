package amd64

// Instruction holds (ADR 0204). A validation kernel can stop one thread at
// every instruction boundary of a short range of its code, so a test can run
// a collection from another thread while the held thread stands at each one.
// The first boundary is an execute breakpoint in DR0; each step after it is
// a single step through RFLAGS.TF, except over the instructions below, which
// run to a breakpoint on the next instruction instead:
//
//   - INT n, INT3, INT1, INTO, SYSCALL and SYSENTER enter the kernel,
//     which runs with TF clear and would see a debug trap in its own code.
//   - PUSHF would push TF into the held code's saved flags, and POPF would
//     load it from them.
//
// A step that leaves the range through a call stops in the callee. When the
// word at the stack pointer is a return address just past the stepped
// instruction and inside the range, the thread runs to a breakpoint there:
// the call is stepped over. Any other exit ends the hold. StepOver refuses to
// step MOV SS, POP SS and IRET at all.

// The RFLAGS bits a hold changes.
const (
	flagTF = uint64(1) << 8
	flagRF = uint64(1) << 16
)

// dr7Local0 enables DR0 as an execute breakpoint of one byte: L0 set, R/W0
// and LEN0 zero.
const dr7Local0 = uint64(1)

// dr6Clear is DR6 with no condition recorded; dr6B0 records a DR0 hit and
// dr6BS a single step.
const (
	dr6Clear = uint64(0xffff0ff0)
	dr6B0    = uint64(1)
	dr6BS    = uint64(1) << 14
)

// maxInstruction is the longest x86 instruction, which bounds how far past
// a call its return address lies.
const maxInstruction = 15

// The states of a hold.
const (
	holdIdle = iota
	// holdRunning: armed, the thread runs to its next stop.
	holdRunning
	// holdStopped: the thread stands at a boundary inside the range.
	holdStopped
	// holdLeft: a step took the thread out of the range.
	holdLeft
)

// StepOver returns the address a thread held at rip runs to as its next
// stop, from the instruction's bytes in code, or zero for a single step.
// Instructions that enter the kernel or move RFLAGS through memory run to a
// breakpoint on the next instruction; most others are single-stepped. Legacy
// prefixes and a REX prefix count. It refuses, answering false:
//
//   - MOV SS and POP SS, which delay a single step's trap past the next
//     instruction, into the kernel when that one is INT or SYSCALL;
//   - IRET, which loads RFLAGS and RIP from memory;
//   - code that ends before the opcode, its ModRM byte or its immediate.
func StepOver(rip uint64, code []byte) (uint64, bool) {
	index := 0
	for index < len(code) && index < maxInstruction && legacyPrefix(code[index]) {
		index++
	}
	if index < len(code) && code[index]&0xf0 == 0x40 {
		index++
	}
	if index >= len(code) || index >= maxInstruction {
		return 0, false
	}
	next := func(length int) (uint64, bool) {
		if index+length > len(code) {
			return 0, false
		}
		return rip + uint64(index+length), true
	}
	switch code[index] {
	case 0x9c, 0x9d, 0xcc, 0xce, 0xf1:
		return next(1)
	case 0xcd:
		return next(2)
	case 0x0f:
		if index+1 >= len(code) {
			return 0, false
		}
		if code[index+1] == 0x05 || code[index+1] == 0x34 {
			return next(2)
		}
	case 0x8e:
		if index+1 >= len(code) || code[index+1]>>3&7 == 2 {
			return 0, false
		}
	case 0x17, 0xcf:
		return 0, false
	}
	return 0, true
}

// legacyPrefix reports whether b is a legacy instruction prefix.
func legacyPrefix(b byte) bool {
	switch b {
	case 0xf0, 0xf2, 0xf3, 0x2e, 0x36, 0x3e, 0x26, 0x64, 0x65, 0x66, 0x67:
		return true
	}
	return false
}

// ReturnInto reports whether ret, the word at the stack pointer of a thread
// that stepped out of [lo, hi) from previous, is the return address of a
// call at previous that lies in the range: the step entered a callee.
func ReturnInto(ret, previous, lo, hi uint64) bool {
	return ret > previous && ret-previous <= maxInstruction && ret >= lo && ret < hi
}

// taskHold is one task's hold. The scheduler lock guards it.
type taskHold struct {
	armed bool
	// stale is what a hold cancelled while its task ran may still raise
	// (staleTrap); that exception resumes the task, any other ends it.
	stale staleHold
	state uint8
	// quiet stops preemption delivery once the first stop came, so a
	// diversion cannot enter the range in the middle of a step.
	quiet    bool
	lo, hi   uint64
	rip      uint64
	previous uint64
	dr0, dr7 uint64
}

// staleHold is what a hold cancelled while its task ran may still raise
// before the task's processor switches: a DR0 hit at breakpoint, or the trap
// of a single step in flight.
type staleHold struct {
	breakpoint uint64
	step       bool
}

// staleOf returns what hold, cancelled while its task runs, may still raise.
func staleOf(hold *taskHold) staleHold {
	stale := staleHold{step: hold.state == holdRunning && hold.dr7 == 0}
	if hold.dr7 != 0 {
		stale.breakpoint = hold.dr0
	}
	return stale
}

// staleTrap reports whether a debug exception with DR6 status at rip is the
// leftover of a cancelled hold: a DR0 hit at its breakpoint, or a single
// step it had in flight. Any other debug exception is the task's own and
// ends it as a fault, as without a hold.
func staleTrap(status, rip uint64, stale staleHold) bool {
	if stale.breakpoint != 0 && status&dr6B0 != 0 && rip == stale.breakpoint {
		return true
	}
	return stale.step && status&dr6BS != 0
}

// endHold decides how a cancel ends hold. A task that stopped or left the
// range is woken, even while it still runs between its debug exception and
// the switch that blocks it: the wake then keeps the block from parking it.
// A task on a processor may still raise its old breakpoint or step, which
// stale records.
func endHold(hold *taskHold, onCPU bool) (stale staleHold, wake bool) {
	wake = hold.state == holdStopped || hold.state == holdLeft
	if onCPU {
		stale = staleOf(hold)
	}
	return stale, wake
}

// armHold arms hold at the first boundary of [lo, hi). It refuses a hold
// already armed: replacing it would leave a stopped task blocked, or a step's
// TF in its flags. The caller cancels the old hold first.
func armHold(hold *taskHold, lo, hi uint64) bool {
	if hold.armed {
		return false
	}
	*hold = taskHold{armed: true, state: holdRunning, lo: lo, hi: hi, dr0: lo, dr7: dr7Local0}
	return true
}

// armFlags returns the saved RFLAGS of a task a hold arms on no processor.
// A resume flag left from a fault would hide the breakpoint at the
// instruction the task resumes at.
func armFlags(flags uint64) uint64 { return flags &^ flagRF }
