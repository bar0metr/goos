package kcall

import "goos/internal/cpuinfo"

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
	// OperationHandleDuplicate a narrower copy of a memory or stream handle (ADR 0107).
	OperationMemoryCreate
	OperationHandleDuplicate
	OperationClockNow
	OperationClockSet
	// OperationExecutableSource creates a pathname-independent executable
	// source from a prepared VFS session described at Arg2.
	OperationExecutableSource
	// OperationConsoleInfo returns read-only terminal geometry and availability
	// flags through an existing console capability.
	OperationConsoleInfo
	OperationCPUInfo
	OperationCPUEntry
	OperationCPUCacheInfo
	// OperationCPURuntime reads one processor's scheduler counters, and
	// OperationTaskAffinity reads or sets the processors a task may run on
	// (ADR 0188).
	OperationCPURuntime
	OperationTaskAffinity
	// OperationIRQTake consumes a line's pending event without blocking; a
	// WaitAny on the line reports the event without consuming it (ADR 0190).
	OperationIRQTake
	// OperationRandomRead fills a buffer of at most MaxRandomRead bytes from
	// the kernel's generator (ADR 0185). It needs no capability.
	OperationRandomRead
	// OperationPowerOffReady checks ACPI S5 support with system authority.
	OperationPowerOffReady
	// OperationLogReadSequence copies an absolute-sequence structured snapshot.
	OperationLogReadSequence
	// The native thread calls (ADR 0203). ThreadCreate starts a thread in the
	// caller's process, ThreadExit ends the calling thread, ThreadJoin waits
	// for one to end, ThreadSelf names the caller, and ThreadYield gives up
	// the processor. ThreadSuspend and ThreadResume are the collector's: they
	// hold every other thread of the caller's process off its processor and
	// let them go again.
	OperationThreadCreate
	OperationThreadExit
	OperationThreadJoin
	OperationThreadSelf
	OperationThreadYield
	OperationThreadSuspend
	OperationThreadResume
	// ThreadSetBase sets the calling thread's FS base, which the native
	// runtime points at the thread's control block. ThreadPreempt registers
	// the runtime's preemption entry for the calling thread, and
	// ThreadPreemptTake hands that entry the instruction pointer it
	// interrupted (ADR 0204).
	OperationThreadSetBase
	OperationThreadPreempt
	OperationThreadPreemptTake
	// ThreadWake sets the wake flag of a thread of the caller's process,
	// which a WaitAny interest on WaitAnyThread reports, and
	// ThreadWakeTake clears the caller's own (ADR 0204).
	OperationThreadWake
	OperationThreadWakeTake
)

// Native thread layout (ADR 0203). A process has ThreadSlots thread slots.
// Slot 0 is the main task on its own stack; slot k from 1 owns a stack of
// ThreadStackSize bytes ending at ThreadStackTop(k), above a ThreadGuardSize
// guard. ThreadAreaTop lies 65 MiB below the top of the main stack, past the
// largest main stack an image may ask for and its guard.
const (
	ThreadSlots     = 16
	ThreadStackSize = uint64(512 << 10)
	ThreadGuardSize = uint64(64 << 10)
	ThreadStride    = ThreadStackSize + ThreadGuardSize
	ThreadAreaTop   = uint64(0x00007fffff000000) - uint64(65<<20)
	// ThreadInfoSize is a thread's ID then its slot.
	ThreadInfoSize = uint64(16)
	// JoinPoll makes ThreadJoin answer Timeout rather than wait.
	JoinPoll = uint64(1)
	// MaxPreemptTicks bounds the interval ThreadPreempt takes, in ticks of
	// the processor's preemption timer.
	MaxPreemptTicks = uint64(1000)
	// PreemptRedZone is how far below the interrupted stack pointer a
	// preemption entry starts: the 128 bytes a leaf function may use below
	// its stack pointer stay untouched.
	PreemptRedZone = uint64(128)
)

// ThreadStackTop returns the stack top of thread slot k, 1 to ThreadSlots-1.
func ThreadStackTop(slot uint64) uint64 { return ThreadAreaTop - (slot-1)*ThreadStride }

// ThreadSlotOf returns the thread slot whose stack holds address, 0 for an
// address above the thread area, and ThreadSlots for one below it.
func ThreadSlotOf(address uint64) uint64 {
	if address >= ThreadAreaTop {
		return 0
	}
	slot := (ThreadAreaTop-1-address)/ThreadStride + 1
	if slot >= ThreadSlots {
		return ThreadSlots
	}
	return slot
}

// A ThreadSuspend snapshot is ThreadSnapshotSize bytes per held thread:
//
//	[0:8]     thread ID
//	[8:16]    slot
//	[16:24]   RSP
//	[24:32]   RIP
//	[32:152]  RAX, RBX, RCX, RDX, RSI, RDI, RBP, R8 to R15
//	[152:408] XMM0 to XMM15
//	[408:416] reserved, zero
const (
	ThreadSnapshotSize      = uint64(416)
	ThreadSnapshotRegisters = uint64(32)
	ThreadSnapshotVectors   = uint64(152)
	// MaxThreadSnapshots bounds one ThreadSuspend: every slot but the
	// caller's.
	MaxThreadSnapshots = uint64(ThreadSlots - 1)
)

// MaxRandomRead bounds one RandomRead.
const MaxRandomRead = uint64(64)

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
	// ActionRetry runs the call again after a scheduling point, with nothing
	// changed: the kernel gave it up before it began, because the task was
	// being stopped while it waited for a lock.
	ActionRetry
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
	LogReadSequence(device uint64, cursor uint64, address uint64) Status
	HandleInfo(slot uint64, address uint64) Status
	ProcessInfo(index uint64, address uint64) Status
	SystemReset(device uint64) Status
	MemoryMap(handle uint64, address uint64, flags uint64) Status
	ProcessKill(process uint64) Status
	ImageMap(device uint64, virtual uint64, address uint64) Status
	LogBounds(device uint64, address uint64) Status
	ConsoleInterrupt(device uint64) Status
	ConsoleInfo(device uint64, address uint64, length uint64) Status
	CPUInfo(address uint64, length uint64) Status
	CPUEntry(index uint64, address uint64) Status
	CPUCacheInfo(index uint64, address uint64) Status
	CPURuntime(index uint64, address uint64) Status
	TaskAffinity(process uint64, mask uint64, address uint64) Status
	IRQTake(irq uint64) Status
	RandomRead(address uint64, length uint64) Status
	ValidationFault(device uint64) Status
	SystemPowerOff(device uint64) Status
	PowerOffReady(device uint64) Status
	ExecutableCreate(device uint64, size uint64, address uint64) Status
	ExecutableWrite(executable uint64, address uint64, length uint64) Status
	ExecutableSource(device uint64, session uint64, descriptor uint64) Status
	VMMap(descriptor uint64, protection uint64, flags uint64) Status
	VMUnmap(address uint64, length uint64) Status
	VMProtect(address uint64, length uint64, protection uint64) Status
	VMDiscard(address uint64, length uint64) Status
	VMQuery(address uint64, record uint64) Status
	MemoryCreate(pages uint64, address uint64) Status
	HandleDuplicate(handle uint64, rights uint64, address uint64) Status
	ClockNow(clock uint64, address uint64, length uint64) Status
	ClockSet(authority uint64, address uint64, length uint64) Status
	ThreadCreate(entry uint64, argument uint64, address uint64) Status
	ThreadExit(code uint64) Status
	// ThreadJoin reports whether the caller must block until the thread
	// ends; the call then runs again.
	ThreadJoin(thread uint64, flags uint64, address uint64) (Status, bool)
	ThreadSelf(address uint64) Status
	ThreadYield() Status
	ThreadSuspend(address uint64, capacity uint64, count uint64) Status
	ThreadResume() Status
	ThreadSetBase(base uint64) Status
	ThreadPreempt(entry uint64, ticks uint64) Status
	ThreadPreemptTake(address uint64) Status
	ThreadWake(thread uint64) Status
	ThreadWakeTake() Status
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
	case OperationLogReadSequence:
		return Result{Status: handler.LogReadSequence(request.Arg0, request.Arg1, request.Arg2)}
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
	case OperationConsoleInfo:
		if request.Arg2 != ConsoleInfoSize {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ConsoleInfo(request.Arg0, request.Arg1, request.Arg2)}
	case OperationCPUInfo:
		if request.Arg1 != CPUInfoSize {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.CPUInfo(request.Arg0, request.Arg1)}
	case OperationCPUEntry:
		return Result{Status: handler.CPUEntry(request.Arg0, request.Arg1)}
	case OperationCPUCacheInfo:
		return Result{Status: handler.CPUCacheInfo(request.Arg0, request.Arg1)}
	case OperationCPURuntime:
		return Result{Status: handler.CPURuntime(request.Arg0, request.Arg1)}
	case OperationTaskAffinity:
		return Result{Status: handler.TaskAffinity(request.Arg0, request.Arg1, request.Arg2)}
	case OperationIRQTake:
		return Result{Status: handler.IRQTake(request.Arg0)}
	case OperationRandomRead:
		if request.Arg1 == 0 || request.Arg1 > MaxRandomRead {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.RandomRead(request.Arg0, request.Arg1)}
	case OperationValidationFault:
		return Result{Status: handler.ValidationFault(request.Arg0)}
	case OperationSystemPowerOff:
		return Result{Status: handler.SystemPowerOff(request.Arg0)}
	case OperationPowerOffReady:
		return Result{Status: handler.PowerOffReady(request.Arg0)}
	case OperationExecutableCreate:
		return Result{Status: handler.ExecutableCreate(request.Arg0, request.Arg1, request.Arg2)}
	case OperationExecutableWrite:
		if request.Arg2 == 0 || request.Arg2 > MaxExecutableWrite {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ExecutableWrite(request.Arg0, request.Arg1, request.Arg2)}
	case OperationExecutableSource:
		return Result{Status: handler.ExecutableSource(request.Arg0, request.Arg1, request.Arg2)}
	case OperationVMMap:
		if request.Arg1&^VMProtectionMask != 0 || request.Arg2&^(VMMapFixed|VMMapStacks) != 0 ||
			request.Arg2&VMMapStacks != 0 && request.Arg1 != VMRead|VMWrite {
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
	case OperationThreadCreate:
		return Result{Status: handler.ThreadCreate(request.Arg0, request.Arg1, request.Arg2)}
	case OperationThreadExit:
		return Result{Status: handler.ThreadExit(request.Arg0), Action: ActionExit}
	case OperationThreadJoin:
		if request.Arg1&^JoinPoll != 0 {
			return Result{Status: StatusInvalidArgument}
		}
		status, blocked := handler.ThreadJoin(request.Arg0, request.Arg1, request.Arg2)
		if blocked {
			return Result{Status: status, Action: ActionBlock}
		}
		return Result{Status: status}
	case OperationThreadSelf:
		return Result{Status: handler.ThreadSelf(request.Arg0)}
	case OperationThreadYield:
		return Result{Status: handler.ThreadYield()}
	case OperationThreadSuspend:
		if request.Arg1 == 0 || request.Arg1 > MaxThreadSnapshots {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ThreadSuspend(request.Arg0, request.Arg1, request.Arg2)}
	case OperationThreadResume:
		return Result{Status: handler.ThreadResume()}
	case OperationThreadSetBase:
		return Result{Status: handler.ThreadSetBase(request.Arg0)}
	case OperationThreadPreempt:
		if request.Arg1 > MaxPreemptTicks || (request.Arg0 == 0) != (request.Arg1 == 0) {
			return Result{Status: StatusInvalidArgument}
		}
		return Result{Status: handler.ThreadPreempt(request.Arg0, request.Arg1)}
	case OperationThreadPreemptTake:
		return Result{Status: handler.ThreadPreemptTake(request.Arg0)}
	case OperationThreadWake:
		return Result{Status: handler.ThreadWake(request.Arg0)}
	case OperationThreadWakeTake:
		return Result{Status: handler.ThreadWakeTake()}
	default:
		return Result{Status: StatusUnsupported}
	}
}

const MaxIPCMessage = uint64(320)
const SystemInfoSize = uint64(40)

const (
	CPUInfoSize      = uint64(cpuinfo.SummarySize)
	CPUEntrySize     = uint64(cpuinfo.CPUEntrySize)
	CPUCacheInfoSize = uint64(cpuinfo.CacheInfoSize)
)

// ConsoleInfoSize is columns, rows, flags, then one reserved uint32.
const ConsoleInfoSize = uint64(16)

const (
	ConsoleFlagSerial      = uint32(1 << 0)
	ConsoleFlagFramebuffer = uint32(1 << 1)
	ConsoleFlagKeyboard    = uint32(1 << 2)
)

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
	// ProcessInfoSelf selects the caller without enumerating other processes.
	ProcessInfoSelf = ^uint64(0)
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
	// VMMapStacks reserves repeated 64 KiB guards and 256 KiB stacks in
	// one anonymous region. Its length must be a multiple of 320 KiB.
	VMMapStacks      = uint64(2)
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
	WaitAnyMaxObjects   = 64
	WaitAnyInterestSize = uint64(16)
	// The high count bit opts into interests with an absolute log cursor.
	WaitAnyCursorFlag         = uint64(1) << 63
	WaitAnyCursorInterestSize = uint64(24)
	WaitAnyResultSize         = uint64(16)
	WaitAnyInfiniteTimeout    = ^uint64(0)
	// WaitAnyThread is the handle of an interest in the calling thread's
	// own wake flag, with WaitReadable; it names no handle-table entry.
	WaitAnyThread = ^uint64(0)
	// WaitAnyThreadEnd marks the handle of an interest in the end of a
	// native thread of the caller's process, with WaitCompleted: the handle
	// is the thread's ID with this bit set (ThreadEndInterest). The bit lies
	// in the index half of a handle ID, above every handle table and task
	// slot, so the encoding names no handle-table entry, and the ID's
	// generation keeps a later thread in the same slot from matching. The
	// interest is ready once the thread has ended, joined or not; ThreadJoin
	// then collects its code. A thread may name its own end, which is never
	// ready while it waits.
	WaitAnyThreadEnd = uint64(1) << 31
)

// ThreadEndInterest returns the WaitAny handle of an interest in the end of
// thread.
func ThreadEndInterest(thread uint64) uint64 { return thread | WaitAnyThreadEnd }

// ThreadEndTarget returns the thread a WaitAny handle names through
// WaitAnyThreadEnd, and false for any other handle.
func ThreadEndTarget(handle uint64) (uint64, bool) {
	if handle == WaitAnyThread || handle&WaitAnyThreadEnd == 0 {
		return 0, false
	}
	return handle &^ WaitAnyThreadEnd, true
}

type WaitEvent uint64

const (
	WaitReadable WaitEvent = 1 << iota
	WaitWritable
	WaitCompleted
	WaitPeerClosed
	WaitInterrupt
)

// LogReadSequence returns a fixed snapshot: oldest, total, first (uint64),
// count (uint32), gap flag (uint32), then count length-prefixed full records.
// Gap marks First advanced to Oldest using wrapping sequence comparison.
const (
	LogReadMax        = 64
	LogReadHeader     = 32
	LogReadRecordSize = 208
	LogReadSize       = LogReadHeader + LogReadMax*LogReadRecordSize
)
