package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestUnsupportedSyscallReturnsENOSYS(t *testing.T) {
	frame := abi.Frame{Number: ^uint64(0)}
	if result := Dispatch(&frame); int64(result) != -38 {
		t.Fatalf("unsupported syscall = %d", int64(result))
	}
	if result := Dispatch(nil); int64(result) != -22 {
		t.Fatalf("nil frame = %d", int64(result))
	}
}
