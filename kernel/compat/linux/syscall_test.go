package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

type testHandler struct{ called abi.Syscall }

func (handler *testHandler) Read(request Request) Result {
	handler.called = request.Number
	return Result{Value: 7}
}
func (handler *testHandler) Write(request Request) Result {
	handler.called = request.Number
	return Result{Errno: abi.EPIPE}
}
func (handler *testHandler) Close(request Request) Result         { return handler.memory(request) }
func (handler *testHandler) Writev(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) Open(request Request) Result          { return handler.memory(request) }
func (handler *testHandler) Openat(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) SetTidAddress(request Request) Result { return handler.memory(request) }
func (handler *testHandler) Brk(request Request) Result           { return handler.memory(request) }
func (handler *testHandler) Mmap(request Request) Result          { return handler.memory(request) }
func (handler *testHandler) Munmap(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) Mprotect(request Request) Result      { return handler.memory(request) }
func (handler *testHandler) Madvise(request Request) Result       { return handler.memory(request) }
func (handler *testHandler) ArchPrctl(request Request) Result     { return handler.memory(request) }
func (handler *testHandler) memory(request Request) Result {
	handler.called = request.Number
	return Result{}
}
func (handler *testHandler) Exit(request Request, group bool) Result {
	handler.called = request.Number
	return Result{Action: abi.ActionExit}
}

func TestUnsupportedSyscallReturnsENOSYS(t *testing.T) {
	unsupportedDiagnostics = UnsupportedDiagnostics{}
	frame := abi.Frame{Number: ^uint64(0), Arg1: 1, Arg2: 2, Arg3: 3, Arg4: 4, Arg5: 5, Arg6: 6}
	if result := Dispatch(&frame, nil); int64(result.Register()) != -38 {
		t.Fatalf("unsupported syscall = %d", int64(result.Register()))
	}
	if result := Dispatch(nil, nil); int64(result.Register()) != -22 {
		t.Fatalf("nil frame = %d", int64(result.Register()))
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
		Dispatch(&abi.Frame{Number: number}, nil)
	}
	diagnostics := Unsupported()
	if diagnostics.Count != unsupportedHistorySize+3 || diagnostics.Stored != unsupportedHistorySize {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if diagnostics.Last() != unsupportedHistorySize+2 || (UnsupportedDiagnostics{}).Last() != 0 {
		t.Fatalf("last unsupported = %d", diagnostics.Last())
	}
	for number := abi.Syscall(3); number < unsupportedHistorySize+3; number++ {
		if diagnostics.Numbers[number%unsupportedHistorySize] != number {
			t.Fatalf("number %d was not retained: %#v", number, diagnostics.Numbers)
		}
	}
}

func TestDispatchRoutesImplementedCalls(t *testing.T) {
	handler := &testHandler{}
	for _, syscall := range []abi.Syscall{
		abi.SysRead, abi.SysWrite, abi.SysOpen, abi.SysClose, abi.SysWritev, abi.SysOpenat,
		abi.SysSetTidAddress, abi.SysBrk, abi.SysMmap, abi.SysMunmap,
		abi.SysMprotect, abi.SysMadvise, abi.SysArchPrctl, abi.SysExit, abi.SysExitGroup,
	} {
		result := Dispatch(&abi.Frame{Number: uint64(syscall)}, handler)
		if handler.called != syscall {
			t.Fatalf("syscall %d routed as %d", syscall, handler.called)
		}
		if syscall == abi.SysRead && result.Register() != 7 {
			t.Fatalf("read result = %d", result.Register())
		}
		if syscall == abi.SysWrite && int64(result.Register()) != -32 {
			t.Fatalf("write result = %d", int64(result.Register()))
		}
	}
}
