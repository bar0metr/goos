package kcall

// MaxConsoleWrite bounds one console write.
const MaxConsoleWrite = uint64(4096)

type Operation uint64

const (
	OperationExit Operation = iota + 1
	// OperationRetiredDebugWrite was DebugWrite, a console write authorized by
	// the caller's identity rather than a capability. It is refused with
	// Unsupported in every build and its number is not reused (ADR 0094).
	OperationRetiredDebugWrite
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
	OperationLogWrite
	OperationLogRead
	OperationHandleInfo
	OperationProcessInfo
	OperationSystemReset
	OperationMemoryMap
	OperationProcessKill
	OperationImageMap
	OperationLogBounds
	OperationConsoleInterrupt
	// OperationValidationFault exists only in the validation build, where it
	// overflows the calling task's kernel stack on purpose so the boot test
	// can see the guard and the double-fault report work (ADR 0091).
	OperationValidationFault
	// OperationSystemPowerOff switches the machine off (ADR 0092).
	OperationSystemPowerOff
	// OperationExecutableCreate and OperationExecutableWrite create and fill
	// an executable object, which a launch can name in place of an extent of
	// the system image (ADR 0097).
	OperationExecutableCreate
	OperationExecutableWrite
	// The native virtual-memory calls (ADR 0104).
	OperationVMMap
	OperationVMUnmap
	OperationVMProtect
	OperationVMDiscard
	OperationVMQuery
	// OperationMemoryCreate creates a shared-memory object and
	// OperationHandleDuplicate a narrower copy of a memory handle (ADR 0107).
	OperationMemoryCreate
	OperationHandleDuplicate
	OperationClockNow
	OperationClockSet
)

// ExitKilled is the exit code of a process stopped through ProcessKill.
const ExitKilled = uint64(1)<<63 | 9

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
	LogWrite(device uint64, address uint64, length uint64) Status
	LogRead(device uint64, index uint64, address uint64) Status
	HandleInfo(slot uint64, address uint64) Status
	ProcessInfo(index uint64, address uint64) Status
	SystemReset(device uint64) Status
	MemoryMap(handle uint64, address uint64, flags uint64) Status
	ProcessKill(process uint64) Status
	ImageMap(device uint64, virtual uint64, address uint64) Status
	LogBounds(device uint64, address uint64) Status
	ConsoleInterrupt(device uint64) Status
	ValidationFault(device uint64) Status
	SystemPowerOff(device uint64) Status
	ExecutableCreate(device uint64, size uint64, address uint64) Status
	ExecutableWrite(executable uint64, address uint64, length uint64) Status
	VMMap(descriptor uint64, protection uint64, flags uint64) Status
	VMUnmap(address uint64, length uint64) Status
	VMProtect(address uint64, length uint64, protection uint64) Status
	VMDiscard(address uint64, length uint64) Status
	VMQuery(address uint64, record uint64) Status
	MemoryCreate(pages uint64, address uint64) Status
	HandleDuplicate(handle uint64, rights uint64, address uint64) Status
	ClockNow(clock uint64, address uint64, length uint64) Status
	ClockSet(authority uint64, address uint64, length uint64) Status
}

// Dispatch validates the architecture-independent native call contract before
// invoking the kernel implementation. Transport-specific register handling is
// deliberately kept in the architecture package.
func Dispatch(request Request, handler Handler) Result {
	switch request.Operation {
	case OperationExit:
		return Result{Status: handler.Exit(request.Arg0), Action: ActionExit}
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
		if request.Arg2 > MaxConsoleWrite {
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
		if request.Arg1 == 0 || request.Arg1 > MaxStartupSize {
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
	case OperationLogWrite:
		if request.Arg2 == 0 || request.Arg2 > MaxLogRecord {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.LogWrite(request.Arg0, request.Arg1, request.Arg2)}
	case OperationLogRead:
		return Result{Status: handler.LogRead(request.Arg0, request.Arg1, request.Arg2)}
	case OperationHandleInfo:
		return Result{Status: handler.HandleInfo(request.Arg0, request.Arg1)}
	case OperationProcessInfo:
		return Result{Status: handler.ProcessInfo(request.Arg0, request.Arg1)}
	case OperationSystemReset:
		return Result{Status: handler.SystemReset(request.Arg0)}
	case OperationProcessKill:
		return Result{Status: handler.ProcessKill(request.Arg0)}
	case OperationImageMap:
		return Result{Status: handler.ImageMap(request.Arg0, request.Arg1, request.Arg2)}
	case OperationLogBounds:
		return Result{Status: handler.LogBounds(request.Arg0, request.Arg1)}
	case OperationConsoleInterrupt:
		return Result{Status: handler.ConsoleInterrupt(request.Arg0)}
	case OperationValidationFault:
		return Result{Status: handler.ValidationFault(request.Arg0)}
	case OperationSystemPowerOff:
		return Result{Status: handler.SystemPowerOff(request.Arg0)}
	case OperationExecutableCreate:
		return Result{Status: handler.ExecutableCreate(request.Arg0, request.Arg1, request.Arg2)}
	case OperationExecutableWrite:
		if request.Arg2 == 0 || request.Arg2 > MaxExecutableWrite {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ExecutableWrite(request.Arg0, request.Arg1, request.Arg2)}
	case OperationVMMap:
		if request.Arg1&^VMProtectionMask != 0 || request.Arg2&^VMMapFixed != 0 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.VMMap(request.Arg0, request.Arg1, request.Arg2)}
	case OperationVMUnmap:
		return Result{Status: handler.VMUnmap(request.Arg0, request.Arg1)}
	case OperationVMProtect:
		if request.Arg2&^VMProtectionMask != 0 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.VMProtect(request.Arg0, request.Arg1, request.Arg2)}
	case OperationVMDiscard:
		return Result{Status: handler.VMDiscard(request.Arg0, request.Arg1)}
	case OperationVMQuery:
		return Result{Status: handler.VMQuery(request.Arg0, request.Arg1)}
	case OperationMemoryCreate:
		return Result{Status: handler.MemoryCreate(request.Arg0, request.Arg1)}
	case OperationHandleDuplicate:
		return Result{Status: handler.HandleDuplicate(request.Arg0, request.Arg1, request.Arg2)}
	case OperationClockNow:
		if request.Arg2 != ClockTimeSize {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ClockNow(request.Arg0, request.Arg1, request.Arg2)}
	case OperationClockSet:
		if request.Arg2 != ClockTimeSize {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ClockSet(request.Arg0, request.Arg1, request.Arg2)}
	case OperationMemoryMap:
		if request.Arg2&^(MemoryMapWritable|MemoryMapAnywhere) != 0 {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.MemoryMap(request.Arg0, request.Arg1, request.Arg2)}
	default:
		return Result{Status: StatusUnsupported}
	}
}

const MaxIPCMessage = uint64(320)
const SystemInfoSize = uint64(40)

// SystemInfoPageSize is the size in bytes of the pages SystemInfo counts.
const SystemInfoPageSize = uint64(4096)
const MaxStreamIO = uint64(256)

// ClockTimeSize is signed seconds, uint32 nanoseconds, then four reserved
// zero bytes.
const ClockTimeSize = uint64(16)

// HandleInfoSize and ProcessInfoSize are the fixed record sizes written by
// HandleInfo and ProcessInfo.
//
// A ProcessInfo record is laid out as:
//
//	[0:8]   process ID
//	[8]     scheduling state
//	[9]     task count
//	[10:16] reserved, zero
//	[16:32] executable name, NUL padded
//	[32:40] CPU ticks charged to the process
//	[40:56] resources held, one uint32 per Resource
//	[56:72] resource limits, same order
//	[72:80] reserved, zero
const (
	HandleInfoSize  = uint64(16)
	ProcessInfoSize = uint64(80)
	MaxProcessName  = 16
	// ProcessResources is the number of counters in each of the held and limit
	// arrays of a ProcessInfo record.
	ProcessResources = 4
	// Offsets into a ProcessInfo record.
	ProcessInfoCPUOffset    = 32
	ProcessInfoUsedOffset   = 40
	ProcessInfoLimitsOffset = 56
)

// Resource indices inside a ProcessInfo record's counter arrays. They match
// the kernel's own ordering and are part of the record's contract.
const (
	ProcessChannels = 0
	ProcessStreams  = 1
	ProcessTasks    = 2
	ProcessPages    = 3
)

// Process states reported by ProcessInfo.
const (
	ProcessRunning = uint8(1)
	ProcessReady   = uint8(2)
	ProcessWaiting = uint8(3)
)

// MaxLogRecord bounds an encoded log record; LogText bounds its text form.
const (
	MaxLogRecord = uint64(181)
	LogText      = uint64(240)
)

// LogBoundsSize is the fixed length of the record LogBounds writes: the
// sequence number of the oldest retained record, then one past the newest.
// A reader keeps a cursor between them; a cursor below the oldest means records
// were overwritten before it read them, and it can say how many were lost
// rather than skipping them silently.
const LogBoundsSize = uint64(16)
const StreamDescriptorSize = uint64(24)

// MemoryMapWritable requests a writable shared-memory mapping.
const MemoryMapWritable = uint64(1)

// MemoryMapAnywhere lets the kernel choose where a shared-memory object is
// mapped; the address argument then names eight bytes of the caller's memory
// that receive the address chosen.
const MemoryMapAnywhere = uint64(2)

// Virtual-memory protections and flags (ADR 0104). A VMMap descriptor is
// sixteen bytes: the address, zero to let the kernel choose unless VMMapFixed
// is set, then the length. The kernel writes the address it used back into the
// first eight bytes.
const (
	VMRead           = uint64(1)
	VMWrite          = uint64(2)
	VMExecute        = uint64(4)
	VMProtectionMask = VMRead | VMWrite | VMExecute
	VMMapFixed       = uint64(1)
	VMDescriptorSize = uint64(16)
)

// A VMQuery record is laid out as:
//
//	[0:8]   base of the region
//	[8:16]  length
//	[16]    protection (VMRead, VMWrite, VMExecute)
//	[17]    backing: 1 anonymous, 2 image, 3 shared, 4 device, 5 guard
//	[18]    flags: bit 0 pinned
//	[19:24] reserved, zero
const VMRecordSize = uint64(24)

// MaxExecutableWrite bounds the bytes one ExecutableWrite appends.
const MaxExecutableWrite = uint64(1 << 20)

// ImageDescriptorSize is the fixed length of the record ImageMap writes back:
// the virtual address the image was mapped at and its length in bytes.
const ImageDescriptorSize = uint64(16)

// MaxStartupSize bounds a process's startup object (ADR 0109). 64 KiB holds a
// PATH_MAX command and working directory and about 56 KiB of arguments and
// environment, more than a shell line can produce, while PID 1's staging area
// stays at 256 KiB for its four slots and the kernel's copy at 64 KiB.
const MaxStartupSize = 64 << 10

// WaitAny is a readiness operation: it never consumes data from a waitable
// object. Userspace performs the typed receive/read/wait operation after the
// returned ready mask identifies the objects whose state changed.
const (
	WaitAnyMaxObjects      = 64
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
