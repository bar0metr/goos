package amd64

// Instruction holds (ADR 0204). A validation kernel can stop one thread at
// every instruction boundary of a short range of its code, so a test can run
// a collection from another thread while the held thread stands at each one.
// The first boundary is an execute breakpoint in DR0; each step after it is
// a single step through RFLAGS.TF, except over the instructions below, which
// run to a breakpoint on the next instruction instead:
//
//   - INT n, INT3, INT1 and SYSCALL enter the kernel, which runs with TF
//     clear and would see a debug trap in its own code.
//   - PUSHF would push TF into the held code's saved flags, and POPF would
//     load it from them.
//
// A step that leaves the range through a call stops in the callee. When the
// word at the stack pointer is a return address just past the stepped
// instruction and inside the range, the thread runs to a breakpoint there:
// the call is stepped over. Any other exit ends the hold.

// The RFLAGS bits a hold changes.
const (
	flagTF = uint64(1) << 8
	flagRF = uint64(1) << 16
)

// dr7Local0 enables DR0 as an execute breakpoint of one byte: L0 set, R/W0
// and LEN0 zero.
const dr7Local0 = uint64(1)

// dr6Clear is DR6 with no condition recorded.
const dr6Clear = uint64(0xffff0ff0)

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
// stop, from the instruction's first bytes in code, or zero for a single
// step. Instructions that enter the kernel or move RFLAGS through memory run
// to a breakpoint on the next instruction; every other one is
// single-stepped. A REX prefix before PUSHF or POPF counts.
func StepOver(rip uint64, code [3]byte) uint64 {
	first, length := code[0], uint64(0)
	if first&0xf0 == 0x40 && (code[1] == 0x9c || code[1] == 0x9d) {
		return rip + 2
	}
	switch first {
	case 0x9c, 0x9d, 0xcc, 0xf1:
		length = 1
	case 0xcd:
		length = 2
	case 0x0f:
		if code[1] == 0x05 {
			length = 2
		}
	}
	if length == 0 {
		return 0
	}
	return rip + length
}

// ReturnInto reports whether ret, the word at the stack pointer of a thread
// that stepped out of [lo, hi) from previous, is the return address of a
// call at previous that lies in the range: the step entered a callee.
func ReturnInto(ret, previous, lo, hi uint64) bool {
	return ret > previous && ret-previous <= maxInstruction && ret >= lo && ret < hi
}
