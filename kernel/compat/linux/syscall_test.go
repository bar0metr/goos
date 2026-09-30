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
func (handler *testHandler) Readv(request Request) Result         { return handler.memory(request) }
func (handler *testHandler) Writev(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) Open(request Request) Result          { return handler.memory(request) }
func (handler *testHandler) Openat(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) SetTidAddress(request Request) Result { return handler.memory(request) }
func (handler *testHandler) Filesystem(request Request) Result    { return handler.memory(request) }
func (handler *testHandler) System(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) Descriptor(request Request) Result    { return handler.memory(request) }
func (handler *testHandler) Brk(request Request) Result           { return handler.memory(request) }
func (handler *testHandler) Mmap(request Request) Result          { return handler.memory(request) }
func (handler *testHandler) Munmap(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) Mprotect(request Request) Result      { return handler.memory(request) }
func (handler *testHandler) Madvise(request Request) Result       { return handler.memory(request) }
func (handler *testHandler) ArchPrctl(request Request) Result     { return handler.memory(request) }
func (handler *testHandler) Execve(request Request) Result        { return handler.memory(request) }
func (handler *testHandler) Fork(request Request) Result          { return handler.memory(request) }
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
	// The history reads oldest first and ends at the newest.
	for index := 0; index < unsupportedHistorySize; index++ {
		if got := diagnostics.Recent(index); got != abi.Syscall(3+index) {
			t.Fatalf("recent %d = %d", index, got)
		}
	}
	if diagnostics.Recent(-1) != 0 || diagnostics.Recent(unsupportedHistorySize) != 0 {
		t.Fatal("an index outside the history returned a number")
	}
	// The refused set keeps the numbers the history already dropped.
	if !diagnostics.WasRefused(0) || !diagnostics.WasRefused(unsupportedHistorySize+2) ||
		diagnostics.WasRefused(unsupportedHistorySize+3) || diagnostics.WasRefused(RefusedNumbers) || diagnostics.WasRefused(^abi.Syscall(0)) {
		t.Fatalf("refused set = %#x", diagnostics.Refused)
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
		abi.SysRead, abi.SysWrite, abi.SysOpen, abi.SysClose, abi.SysStat, abi.SysFstat,
		abi.SysLstat, abi.SysNewfstatat, abi.SysReadv, abi.SysWritev, abi.SysOpenat,
		abi.SysSetTidAddress, abi.SysLseek, abi.SysGetcwd, abi.SysChdir, abi.SysRename,
		abi.SysMkdir, abi.SysRmdir, abi.SysUnlink, abi.SysMkdirat, abi.SysUnlinkat,
		abi.SysRenameat, abi.SysRenameat2, abi.SysGetdents64, abi.SysGetpid, abi.SysGettid,
		abi.SysGetppid, abi.SysGetuid, abi.SysGeteuid, abi.SysGetgid, abi.SysGetegid, abi.SysUname,
		abi.SysClockGettime, abi.SysNanosleep, abi.SysPrlimit64, abi.SysDup, abi.SysDup2,
		abi.SysDup3, abi.SysFcntl, abi.SysIoctl, abi.SysAccess, abi.SysFaccessat, abi.SysSetuid,
		abi.SysSetgid, abi.SysGetgroups, abi.SysBrk, abi.SysMmap, abi.SysMunmap,
		abi.SysMprotect, abi.SysMadvise, abi.SysArchPrctl, abi.SysExit, abi.SysExitGroup,
		abi.SysPread64, abi.SysPwrite64, abi.SysStatx, abi.SysRtSigaction, abi.SysRtSigprocmask, abi.SysGetrandom, abi.SysExecve,
		abi.SysFork,
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

func TestBadPointerCounterSaturates(t *testing.T) {
	badPointers, lastBadPointer = 0, 0
	NoteBadPointer(abi.SysRead)
	if count, last := BadPointers(); count != 1 || last != abi.SysRead {
		t.Fatalf("bad pointers = %d, last call %d", count, last)
	}
	badPointers = ^uint64(0)
	NoteBadPointer(abi.SysWritev)
	if count, last := BadPointers(); count != ^uint64(0) || last != abi.SysWritev {
		t.Fatalf("the counter wrapped: %d, last call %d", count, last)
	}
	badPointers, lastBadPointer = 0, 0
}
