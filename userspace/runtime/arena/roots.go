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
	native.markRange(uintptr(unsafe.Pointer(&registers[0])), uintptr(unsafe.Pointer(&registers[0]))+unsafe.Sizeof(registers))
	native.markRange(sp, stackTop)
	registers = [6]uintptr{}
}
