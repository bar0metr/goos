//go:build goos_userspace

package arena

import "unsafe"

// The roots of a collection are the mutable globals, the stack of the one
// thread a native program runs, the callee-saved registers, and the used part
// of every region. TinyGo's own stack scanner is not built for this target
// (it serves gc.custom only on WebAssembly), so the runtime finds all of them
// itself: the linker script brackets the globals, and _start records the top
// of the stack.

//go:extern _goos_gc_globals_start
var globalsStart [0]byte

//go:extern _goos_gc_globals_end
var globalsEnd [0]byte

//go:extern goos_stack_top
var stackTop uintptr

//go:linkname spillRegisters goos_gc_registers
func spillRegisters(buffer *[6]uintptr) uintptr

//go:linkname scrubStack goos_gc_scrub
func scrubStack(limit uintptr)

// stackReserve returns _goos_stack_reserve, the stack a program asks the
// kernel for, an absolute symbol the linker script sets and elf2native
// records (ADR 0104).
//
//go:linkname stackReserve goos_stack_reserve
func stackReserve() uintptr

// stackBase is the lowest address of the stack. The kernel reserves the
// stack as the stackReserve bytes that end at the page _start was entered
// in, with an unmapped guard below (kernel/loader setUpStack).
func stackBase() uintptr {
	reserve := stackReserve()
	if stackTop == 0 || reserve == 0 || reserve&(pageBytes-1) != 0 || reserve > stackTop {
		panic("arena: stack bounds unknown")
	}
	return (stackTop - reserve + pageBytes - 1) &^ (pageBytes - 1)
}

// probeRegisters receives the registers StackRoom spills to find the stack
// pointer; it is cleared at once, since globals are roots.
var probeRegisters [6]uintptr

// StackRoom returns how many bytes of stack lie below its caller's frame.
// Validation programs use it to run code near the bottom of the stack.
//
//go:noinline
func StackRoom() uintptr {
	sp := spillRegisters(&probeRegisters)
	probeRegisters = [6]uintptr{}
	return sp - stackBase()
}

// registers holds the callee-saved registers while a collection runs.
var registers [6]uintptr

// markGlobals scans the globals, skipping the heap's own state: its base
// address would otherwise keep the first allocation alive forever.
func markGlobals() {
	start := uintptr(unsafe.Pointer(&globalsStart))
	end := uintptr(unsafe.Pointer(&globalsEnd))
	self := uintptr(unsafe.Pointer(&native))
	selfEnd := self + unsafe.Sizeof(native)
	if self < start || selfEnd > end {
		panic("arena: heap state outside the globals range")
	}
	native.markRange(start, self)
	native.markRange(selfEnd, end)
}

// markStack scans the registers and every frame from the collector's caller
// up to the top of the stack.
func markStack() {
	sp := spillRegisters(&registers)
	if stackTop == 0 || sp > stackTop {
		panic("arena: stack top unknown")
	}
	native.phase = PhaseRegisters
	native.markRange(uintptr(unsafe.Pointer(&registers[0])), uintptr(unsafe.Pointer(&registers[0]))+unsafe.Sizeof(registers))
	native.phase = PhaseStack
	native.markRange(sp, stackTop)
	registers = [6]uintptr{}
}

// Token returns an allocation's address with every bit inverted, so holding
// it does not keep the allocation alive.
func Token[T any](value *T) uint64 { return ^uint64(uintptr(unsafe.Pointer(value))) }

// Phases of a collection, as Watched reports them.
const (
	PhaseNone = iota
	PhaseGlobals
	PhaseRegions
	PhaseRegisters
	PhaseStack
)

// Watch records, during the following collections, the first word that
// marks the allocation a token names. It is not inlined: the lookup would
// otherwise leave the allocation's address in the caller's frame.
//
//go:noinline
func Watch(token uint64) {
	native.watch, native.watchAt, native.watchPhase = 0, 0, PhaseNone
	if _, _, start, ok := native.find(uintptr(^token)); ok {
		native.watch = start
	}
}

// Watched reports the root phase and the address of the word that marked the
// watched allocation, and for a word on the stack the nearest words below and
// above it that look like return addresses, which name its frame.
func Watched() (phase int, at, below, above uintptr) {
	phase, at = int(native.watchPhase), native.watchAt
	if phase == PhaseStack && at != 0 {
		below, above = codeWord(at, 0, -1), codeWord(at, stackTop, 1)
	}
	return phase, at, below, above
}

// codeWord returns the first word from address towards limit that lies in
// the program's code, between the image base and its globals.
func codeWord(address, limit uintptr, direction int) uintptr {
	code := uintptr(unsafe.Pointer(&globalsStart))
	for steps := 0; steps < 4096 && address != limit; steps++ {
		if direction < 0 {
			address -= wordBytes
		} else {
			address += wordBytes
		}
		if address == limit {
			break
		}
		if value := *(*uintptr)(pointerAt(address)); value >= 0x40002000 && value < code {
			return value
		}
	}
	return 0
}
