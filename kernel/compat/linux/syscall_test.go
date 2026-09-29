package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestUnsupportedSyscallReturnsENOSYS(t *testing.T) {
	unsupportedDiagnostics = UnsupportedDiagnostics{}
	frame := abi.Frame{Number: ^uint64(0), Arg1: 1, Arg2: 2, Arg3: 3, Arg4: 4, Arg5: 5, Arg6: 6}
	if result := Dispatch(&frame); int64(result) != -38 {
		t.Fatalf("unsupported syscall = %d", int64(result))
	}
	if result := Dispatch(nil); int64(result) != -22 {
		t.Fatalf("nil frame = %d", int64(result))
	}
	diagnostics := Unsupported()
	if diagnostics.Count != 1 || diagnostics.Stored != 1 || diagnostics.Numbers[0] != ^abi.Syscall(0) {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestDecodeCopiesEveryScalarArgument(t *testing.T) {
	frame := abi.Frame{Number: uint64(abi.SysOpenat), Arg1: 1, Arg2: 2, Arg3: 3, Arg4: 4, Arg5: 5, Arg6: 6}
	request, ok := decode(&frame)
	if !ok || request.Number != abi.SysOpenat || request.Arguments != [6]uint64{1, 2, 3, 4, 5, 6} {
		t.Fatalf("request = %#v ok=%v", request, ok)
	}
}

func TestUnsupportedDiagnosticsStayBounded(t *testing.T) {
	unsupportedDiagnostics = UnsupportedDiagnostics{}
	for number := uint64(0); number < unsupportedHistorySize+3; number++ {
		Dispatch(&abi.Frame{Number: number})
	}
	diagnostics := Unsupported()
	if diagnostics.Count != unsupportedHistorySize+3 || diagnostics.Stored != unsupportedHistorySize {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	for number := abi.Syscall(3); number < unsupportedHistorySize+3; number++ {
		if diagnostics.Numbers[number%unsupportedHistorySize] != number {
			t.Fatalf("number %d was not retained: %#v", number, diagnostics.Numbers)
		}
	}
}
