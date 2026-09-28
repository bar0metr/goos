//go:build uefi && amd64

package main

import "goos/kernel/keyboard"

var consoleKeyboard keyboard.Decoder

// keyboardFeed decodes one scan-code byte, stores the terminal bytes it
// produces in target, and returns their count in bits 0..7 and the logical
// console-interrupt flag in bit 32. It is allocation-free and is called with
// interrupts disabled by the IRQ1 entry, which owns target.
//
//export goos_keyboard_feed
func keyboardFeed(scanCode byte, target *[keyboard.MaxBytes]byte) uint64 {
	output := consoleKeyboard.Feed(scanCode)
	*target = output.Bytes
	result := uint64(output.Length)
	if output.Interrupt {
		result |= 1 << 32
	}
	return result
}
