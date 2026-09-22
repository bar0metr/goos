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
	OperationPortRead
	OperationPortWrite
	OperationDMAAllocate
	OperationIRQWait
	OperationStreamCreate
	OperationStreamRead
	OperationStreamWrite
	OperationStreamWait
	OperationWaitAny
	OperationDiagnostics
	OperationClockReadNanos
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
	PortRead(device uint64, specification uint64, address uint64) Status
	PortWrite(device uint64, specification uint64, value uint64) Status
	DMAAllocate(device uint64, virtual uint64, address uint64) Status
	StreamCreate(address uint64, length uint64) Status
	StreamRead(stream uint64, descriptor uint64, length uint64) Status
	StreamWrite(stream uint64, descriptor uint64, length uint64) Status
	Diagnostics(device uint64, address uint64) Status
	ClockReadNanos(address uint64, length uint64) Status
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
	case OperationPortRead:
		return Result{Status: handler.PortRead(request.Arg0, request.Arg1, request.Arg2)}
	case OperationPortWrite:
		return Result{Status: handler.PortWrite(request.Arg0, request.Arg1, request.Arg2)}
	case OperationDMAAllocate:
		return Result{Status: handler.DMAAllocate(request.Arg0, request.Arg1, request.Arg2)}
	case OperationStreamCreate:
		if request.Arg1 != 16 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.StreamCreate(request.Arg0, request.Arg1)}
	case OperationStreamRead:
		if request.Arg2 != StreamDescriptorSize {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.StreamRead(request.Arg0, request.Arg1, request.Arg2)}
	case OperationStreamWrite:
		if request.Arg2 != StreamDescriptorSize {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.StreamWrite(request.Arg0, request.Arg1, request.Arg2)}
	case OperationDiagnostics:
		return Result{Status: handler.Diagnostics(request.Arg0, request.Arg1)}
	case OperationClockReadNanos:
		if request.Arg1 != 8 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ClockReadNanos(request.Arg0, request.Arg1)}
	default:
		return Result{Status: StatusUnsupported}
	}
}

const MaxIPCMessage = uint64(320)
const SystemInfoSize = uint64(40)
const MaxStreamIO = uint64(256)
const StreamDescriptorSize = uint64(24)

// WaitAny is a readiness operation: it never consumes data from a waitable
// object. Userspace performs the typed receive/read/wait operation after the
// returned ready mask identifies the objects whose state changed.
const (
	WaitAnyMaxObjects      = 16
	WaitAnyInterestSize    = uint64(16)
	WaitAnyResultSize      = uint64(16)
	WaitAnyInfiniteTimeout = ^uint64(0)
)

type WaitEvent uint64

const (
	WaitReadable WaitEvent = 1 << iota
	WaitWritable
	WaitCompleted
	WaitPeerClosed
	WaitInterrupt
)
