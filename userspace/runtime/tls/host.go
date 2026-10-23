//go:build !goos_userspace

package tls

// A host build runs as the main thread.

var hostBlock = Block{Magic: Magic}

// Slot returns 0 on the host.
func Slot() int { return 0 }

// Pin returns 0 on the host.
func Pin() int { return 0 }

// Unpin does nothing on the host.
func Unpin() {}

// Current returns the host's single block.
func Current() *Block { return &hostBlock }

// Of returns the host's single block for every slot.
func Of(int) *Block { return &hostBlock }
