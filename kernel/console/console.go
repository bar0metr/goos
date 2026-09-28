//go:build uefi && amd64

// Package console mirrors the kernel's logical output to serial and, once the
// permanent GOP mapping exists, to the framebuffer terminal.
package console

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/framebuffer"
)

var (
	serial       = amd64.NewCOM1()
	graphical    framebuffer.Terminal
	graphicsLive bool
)

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
	serial.WriteByte(value)
	if graphicsLive {
		graphical.ConsumeByte(value)
		graphical.RefreshValidationMarker()
	}
}

// Write preserves the byte-exact ConsoleWrite ABI and refreshes validation
// overlays once after the complete batch.
func (Writer) Write(value []byte) {
	for _, item := range value {
		serial.WriteByte(item)
	}
	if graphicsLive {
		graphical.Write(value)
		graphical.RefreshValidationMarker()
	}
}

func (Writer) WriteString(value string) {
	serial.WriteString(value)
	if !graphicsLive {
		return
	}
	for index := 0; index < len(value); index++ {
		graphical.ConsumeByte(value[index])
	}
	graphical.RefreshValidationMarker()
}

// Panic writes the complete diagnostic to serial before attempting graphics.
func Panic(message string) {
	serial.WriteString("PANIC: ")
	serial.WriteString(message)
	serial.WriteString("\n")
	if graphicsLive {
		graphical.WriteString("PANIC: ")
		graphical.WriteString(message)
		graphical.WriteString("\n")
		graphical.RefreshValidationMarker()
	}
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
