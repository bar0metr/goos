package kcall

const MaxDebugWrite = uint64(4096)

type Operation uint64

const (
	OperationExit Operation = iota + 1
	OperationDebugWrite
	OperationIPCSend
	OperationIPCReceive
)

type Status uint64

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusPermissionDenied
	StatusNotFound
	StatusAlreadyExists
	StatusResourceExhausted
	StatusTimeout
	StatusPeerDied
	StatusUnsupported
	StatusIOError
	StatusCorruptData
)

type Action uint8

const (
	ActionReturn Action = iota
	ActionExit
)

type Request struct {
	Operation Operation
	Arg0      uint64
	Arg1      uint64
	Arg2      uint64
}

type Result struct {
	Status Status
	Value  uint64
	Action Action
}

type Handler interface {
	Exit(code uint64) Status
	DebugWrite(address uint64, length uint64) Status
	IPCSend(channel uint64, address uint64, length uint64) Status
	IPCReceive(channel uint64, address uint64, capacity uint64) Status
}

// Dispatch validates the architecture-independent native call contract before
// invoking the kernel implementation. Transport-specific register handling is
// deliberately kept in the architecture package.
func Dispatch(request Request, handler Handler) Result {
	switch request.Operation {
	case OperationExit:
		return Result{Status: handler.Exit(request.Arg0), Action: ActionExit}
	case OperationDebugWrite:
		if request.Arg1 > MaxDebugWrite {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.DebugWrite(request.Arg0, request.Arg1)}
	case OperationIPCSend:
		if request.Arg2 > MaxIPCMessage {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.IPCSend(request.Arg0, request.Arg1, request.Arg2)}
	case OperationIPCReceive:
		if request.Arg2 > MaxIPCMessage {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.IPCReceive(request.Arg0, request.Arg1, request.Arg2)}
	default:
		return Result{Status: StatusUnsupported}
	}
}

const MaxIPCMessage = uint64(320)
