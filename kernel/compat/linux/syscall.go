package linux

import "goos/kernel/compat/linux/abi"

const unsupportedHistorySize = 8

const (
	// MaxIOVec bounds the vector one readv or writev call may pass.
	MaxIOVec = 16
	// MaxVectorBytes bounds what one vector call transfers. A longer vector
	// is a short transfer, which the caller continues.
	MaxVectorBytes = 64 << 10
)

// Request is the scalar Linux register ABI after architectural entry checks.
// Pointer arguments remain integers until an operation-specific decoder copies
// them through validated user-memory helpers.
type Request struct {
	Number    abi.Syscall
	Arguments [6]uint64
}

type Result struct {
	Value  uint64
	Errno  abi.Errno
	Action uint64
}

func (result Result) Register() uint64 {
	if result.Errno != 0 {
		return abi.Negative(result.Errno)
	}
	return result.Value
}

type Handler interface {
	Read(Request) Result
	Write(Request) Result
	Close(Request) Result
	Writev(Request) Result
	Openat(Request) Result
	SetTidAddress(Request) Result
	Brk(Request) Result
	Mmap(Request) Result
	Munmap(Request) Result
	Mprotect(Request) Result
	Madvise(Request) Result
	ArchPrctl(Request) Result
	Exit(Request, bool) Result
}

type UnsupportedDiagnostics struct {
	Count   uint64
	Numbers [unsupportedHistorySize]abi.Syscall
	Stored  uint8
}

var unsupportedDiagnostics UnsupportedDiagnostics

func decode(frame *abi.Frame) (Request, bool) {
	if frame == nil {
		return Request{}, false
	}
	return Request{Number: abi.Syscall(frame.Number), Arguments: [6]uint64{frame.Arg1, frame.Arg2, frame.Arg3, frame.Arg4, frame.Arg5, frame.Arg6}}, true
}

// Dispatch is the bounded Linux syscall translation boundary. Implemented
// calls remain separate from the native kcall namespace; unsupported calls
// are recorded without emitting one log entry per invocation.
func Dispatch(frame *abi.Frame, handler Handler) Result {
	request, ok := decode(frame)
	if !ok {
		return Result{Errno: abi.EINVAL}
	}
	if handler != nil {
		switch request.Number {
		case abi.SysRead:
			return handler.Read(request)
		case abi.SysWrite:
			return handler.Write(request)
		case abi.SysClose:
			return handler.Close(request)
		case abi.SysWritev:
			return handler.Writev(request)
		case abi.SysOpenat:
			return handler.Openat(request)
		case abi.SysSetTidAddress:
			return handler.SetTidAddress(request)
		case abi.SysBrk:
			return handler.Brk(request)
		case abi.SysMmap:
			return handler.Mmap(request)
		case abi.SysMunmap:
			return handler.Munmap(request)
		case abi.SysMprotect:
			return handler.Mprotect(request)
		case abi.SysMadvise:
			return handler.Madvise(request)
		case abi.SysArchPrctl:
			return handler.ArchPrctl(request)
		case abi.SysExit:
			return handler.Exit(request, false)
		case abi.SysExitGroup:
			return handler.Exit(request, true)
		}
	}
	noteUnsupported(request.Number)
	return Result{Errno: abi.ENOSYS}
}

func noteUnsupported(number abi.Syscall) {
	if unsupportedDiagnostics.Count != ^uint64(0) {
		unsupportedDiagnostics.Count++
	}
	index := unsupportedDiagnostics.Count - 1
	if index >= unsupportedHistorySize {
		index %= unsupportedHistorySize
	}
	unsupportedDiagnostics.Numbers[index] = number
	if unsupportedDiagnostics.Stored < unsupportedHistorySize {
		unsupportedDiagnostics.Stored++
	}
}

func Unsupported() UnsupportedDiagnostics { return unsupportedDiagnostics }

// Last returns the most recent unsupported number, or zero before the first.
func (diagnostics UnsupportedDiagnostics) Last() abi.Syscall {
	if diagnostics.Count == 0 {
		return 0
	}
	return diagnostics.Numbers[(diagnostics.Count-1)%unsupportedHistorySize]
}
