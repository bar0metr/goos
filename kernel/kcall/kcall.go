package kcall

const MaxDebugWrite = uint64(4096)

type Operation uint64

const (
	OperationExit Operation = iota + 1
	OperationDebugWrite
	OperationIPCSend
	OperationIPCReceive
	OperationIPCWaitReceive
	OperationConsoleWrite
	OperationConsoleRead
	OperationClockRead
	OperationStartupRead
	OperationChannelCreate
	OperationHandleClose
	OperationProcessWait
	OperationSystemInfo
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
	ActionBlock
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
	ConsoleWrite(device uint64, address uint64, length uint64) Status
	ConsoleRead(device uint64, address uint64, length uint64) Status
	ClockRead(address uint64, length uint64) Status
	StartupRead(address uint64, length uint64) Status
	ChannelCreate(address uint64, length uint64) Status
	HandleClose(id uint64) Status
	SystemInfo(address uint64, length uint64) Status
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
	case OperationConsoleWrite:
		if request.Arg2 > MaxDebugWrite {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ConsoleWrite(request.Arg0, request.Arg1, request.Arg2)}
	case OperationConsoleRead:
		if request.Arg2 != 1 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ConsoleRead(request.Arg0, request.Arg1, request.Arg2)}
	case OperationClockRead:
		if request.Arg1 != 8 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ClockRead(request.Arg0, request.Arg1)}
	case OperationStartupRead:
		if request.Arg1 != 128 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.StartupRead(request.Arg0, request.Arg1)}
	case OperationChannelCreate:
		if request.Arg1 != 16 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ChannelCreate(request.Arg0, request.Arg1)}
	case OperationHandleClose:
		return Result{Status: handler.HandleClose(request.Arg0)}
	case OperationSystemInfo:
		if request.Arg1 != SystemInfoSize {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.SystemInfo(request.Arg0, request.Arg1)}
	default:
		return Result{Status: StatusUnsupported}
	}
}

const MaxIPCMessage = uint64(320)
const SystemInfoSize = uint64(40)
