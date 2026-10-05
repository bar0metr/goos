//go:build uefi && amd64

// Package console mirrors the kernel's logical output to serial and, once the
// permanent GOP mapping exists, to the framebuffer terminal.
package console

import (
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	"goos/kernel/framebuffer"
	"goos/kernel/lock"
)

var (
	serial       = amd64.NewCOM1()
	graphical    framebuffer.Terminal
	graphicsLive bool
)

// Kernel text reaches the devices a line at a time. Each processor collects
// its own line, with local interrupts disabled, and emits it whole under
// outputLock, so lines from different processors never interleave. Bytes a
// process writes are emitted as the one block it wrote.
const lineCapacity = 256

type pendingLine struct {
	data   [lineCapacity]byte
	length int
}

var (
	outputLock = lock.New(lock.LevelConsole)
	lines      [amd64.MaxCPUs]pendingLine
)

// panicSpins bounds how long a panic waits for a processor that stopped
// while it held the output lock.
const panicSpins = 1 << 22

func appendByte(value byte) {
	line := &lines[amd64.CPUIndex()]
	line.data[line.length] = value
	line.length++
	if value == '\n' || line.length == lineCapacity {
		flushLine(line)
	}
}

func flushLine(line *pendingLine) {
	if line.length == 0 {
		return
	}
	outputLock.Lock()
	emit(line.data[:line.length])
	outputLock.Unlock()
	line.length = 0
}

func emit(value []byte) {
	for _, item := range value {
		serial.WriteByte(item)
	}
	if graphicsLive {
		graphical.Write(value)
		graphical.RefreshValidationMarker()
	}
}

type Writer struct{}

func New() Writer { return Writer{} }

func (Writer) Init() { serial.Init() }

// AttachFramebuffer clears the framebuffer to the terminal's default style
// and enables mirroring for subsequent writes.
func AttachFramebuffer(buffer framebuffer.Buffer) bool {
	terminal, ok := framebuffer.NewTerminal(buffer)
	if !ok {
		return false
	}
	graphical = terminal
	graphicsLive = true
	return true
}

// EnableValidationMarker pins the validation-only framebuffer checkerboard.
func EnableValidationMarker() bool {
	return graphicsLive && graphical.EnableValidationMarker()
}

func (Writer) WriteByte(value byte) {
	flags := amd64.DisableInterrupts()
	appendByte(value)
	amd64.RestoreInterrupts(flags)
}

// Write preserves the byte-exact ConsoleWrite ABI and refreshes validation
// overlays once after the complete batch.
func (Writer) Write(value []byte) {
	flags := amd64.DisableInterrupts()
	flushLine(&lines[amd64.CPUIndex()])
	outputLock.Lock()
	emit(value)
	outputLock.Unlock()
	amd64.RestoreInterrupts(flags)
}

func (Writer) WriteString(value string) {
	flags := amd64.DisableInterrupts()
	for index := 0; index < len(value); index++ {
		appendByte(value[index])
	}
	amd64.RestoreInterrupts(flags)
}

// Panic writes the complete diagnostic to serial before attempting graphics.
// The other processors are stopped; one of them may have held the output
// lock, so the panic waits for it only a bounded time.
func Panic(message string) {
	amd64.DisableInterrupts()
	line := &lines[amd64.CPUIndex()]
	locked := false
	// A validation build knows when this processor already holds the lock:
	// the panic came from inside the console.
	mine := validation.Enabled && outputLock.HeldByCaller()
	for spins := 0; spins < panicSpins && !mine; spins++ {
		if locked = outputLock.TryLock(); locked {
			break
		}
	}
	for _, item := range line.data[:line.length] {
		serial.WriteByte(item)
	}
	line.length = 0
	serial.WriteString("PANIC: ")
	serial.WriteString(message)
	serial.WriteString("\n")
	if graphicsLive {
		graphical.WriteString("PANIC: ")
		graphical.WriteString(message)
		graphical.WriteString("\n")
		graphical.RefreshValidationMarker()
	}
	if locked {
		outputLock.Unlock()
	}
}

// WriteSerial writes to COM1 alone: the serial terminal console (ADR 0125).
func WriteSerial(value []byte) {
	flags := amd64.DisableInterrupts()
	outputLock.Lock()
	for _, item := range value {
		serial.WriteByte(item)
	}
	outputLock.Unlock()
	amd64.RestoreInterrupts(flags)
}

// WriteGraphical writes to the framebuffer terminal alone, if there is one.
func WriteGraphical(value []byte) {
	if !graphicsLive {
		return
	}
	flags := amd64.DisableInterrupts()
	outputLock.Lock()
	graphical.Write(value)
	graphical.RefreshValidationMarker()
	outputLock.Unlock()
	amd64.RestoreInterrupts(flags)
}

func (writer Writer) WriteUint(value uint64) {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	for ; position < len(digits); position++ {
		writer.WriteByte(digits[position])
	}
}

func (writer Writer) WriteHex(value uint64) {
	const digits = "0123456789abcdef"
	writer.WriteString("0x")
	started := false
	for shift := 60; shift >= 0; shift -= 4 {
		nibble := byte(value >> uint(shift) & 0xf)
		if nibble != 0 || started || shift == 0 {
			writer.WriteByte(digits[nibble])
			started = true
		}
	}
}
