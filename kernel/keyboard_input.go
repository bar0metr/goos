//go:build uefi && amd64

package main

import "goos/kernel/keyboard"

var consoleKeyboard keyboard.Decoder

// keyboardFeed returns up to three terminal bytes in bits 0..23, their count
// in bits 24..25, and the logical console-interrupt flag in bit 32. It is
// allocation-free and is called with interrupts disabled by the IRQ1 entry.
//
//export goos_keyboard_feed
func keyboardFeed(scanCode byte) uint64 {
	output := consoleKeyboard.Feed(scanCode)
	result := uint64(output.Bytes[0]) |
		uint64(output.Bytes[1])<<8 |
		uint64(output.Bytes[2])<<16 |
		uint64(output.Length)<<24
	if output.Interrupt {
		result |= 1 << 32
	}
	return result
}
