//go:build uefi && amd64

package amd64

// ReadTSC returns the processor time-stamp counter. It is not serializing, so
// it suits intervals of microseconds and longer, not single instructions.
//
//export goos_read_tsc
func ReadTSC() uint64
