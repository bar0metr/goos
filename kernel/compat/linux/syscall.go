package linux

import "goos/kernel/compat/linux/abi"

const unsupportedHistorySize = 8

// Request is the scalar Linux register ABI after architectural entry checks.
// Pointer arguments remain integers until an operation-specific decoder copies
// them through validated user-memory helpers.
type Request struct {
	Number    abi.Syscall
	Arguments [6]uint64
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

// Dispatch is the bounded Linux syscall translation boundary. Phase 7 owns
// number decoding and unsupported-call diagnostics. Semantic handlers replace
// ENOSYS in their own phases without entering the native kcall namespace.
func Dispatch(frame *abi.Frame) uint64 {
	request, ok := decode(frame)
	if !ok {
		return abi.Negative(abi.EINVAL)
	}
	noteUnsupported(request.Number)
	return abi.Negative(abi.ENOSYS)
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
