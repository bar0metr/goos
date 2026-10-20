package kcall

import "testing"

type testHandler struct {
	exitCode uint64
	address  uint64
	length   uint64
	channel  uint64
	called   bool
}

func (handler *testHandler) IPCSend(channel uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = channel, address, length
	return StatusOK
}

func (handler *testHandler) IPCReceive(channel uint64, address uint64, capacity uint64) Status {
	handler.channel, handler.address, handler.length = channel, address, capacity
	return StatusOK
}

func (handler *testHandler) ConsoleWrite(device uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = device, address, length
	return StatusOK
}

func (handler *testHandler) ConsoleRead(device uint64, address uint64, length uint64) Status {
	handler.called = true
	return StatusOK
}

func (handler *testHandler) ClockRead(address uint64, length uint64) Status {
	handler.called = true
	return StatusOK
}

func (handler *testHandler) StartupRead(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) ChannelCreate(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) HandleClose(id uint64) Status {
	handler.channel = id
	return StatusOK
}

func (handler *testHandler) SystemInfo(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) CPUInfo(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) CPUEntry(index uint64, address uint64) Status {
	handler.length, handler.address = index, address
	return StatusOK
}

func (handler *testHandler) CPUCacheInfo(index uint64, address uint64) Status {
	handler.length, handler.address = index, address
	return StatusOK
}

func (handler *testHandler) CPURuntime(index uint64, address uint64) Status {
	handler.length, handler.address = index, address
	return StatusOK
}

func (handler *testHandler) TaskAffinity(process uint64, mask uint64, address uint64) Status {
	handler.length, handler.address = mask, address
	return StatusOK
}

func (handler *testHandler) IRQTake(irq uint64) Status {
	handler.channel = irq
	return StatusOK
}

func (handler *testHandler) RandomRead(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) PortRead(device uint64, specification uint64, address uint64) Status {
	return StatusOK
}
func (handler *testHandler) PortWrite(device uint64, specification uint64, value uint64) Status {
	return StatusOK
}
func (handler *testHandler) DMAAllocate(device uint64, virtual uint64, address uint64) Status {
	return StatusOK
}
func (handler *testHandler) StreamCreate(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}
func (handler *testHandler) StreamRead(stream uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = stream, address, length
	return StatusOK
}
func (handler *testHandler) StreamWrite(stream uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = stream, address, length
	return StatusOK
}

func (handler *testHandler) Diagnostics(device uint64, address uint64) Status {
	handler.channel, handler.address = device, address
	return StatusUnsupported
}

func (handler *testHandler) LogWrite(device uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = device, address, length
	return StatusOK
}

func (handler *testHandler) LogRead(device uint64, index uint64, address uint64) Status {
	handler.channel, handler.length, handler.address = device, index, address
	return StatusOK
}

func (handler *testHandler) HandleInfo(slot uint64, address uint64) Status {
	handler.length, handler.address = slot, address
	return StatusOK
}

func (handler *testHandler) ProcessInfo(index uint64, address uint64) Status {
	handler.length, handler.address = index, address
	return StatusOK
}

func (handler *testHandler) SystemReset(device uint64) Status {
	handler.channel = device
	return StatusPermissionDenied
}

func (handler *testHandler) MemoryMap(memoryHandle uint64, address uint64, flags uint64) Status {
	handler.channel, handler.address, handler.length = memoryHandle, address, flags
	return StatusOK
}

func (handler *testHandler) ProcessKill(process uint64) Status {
	handler.channel = process
	return StatusOK
}

func (handler *testHandler) ImageMap(device uint64, virtual uint64, address uint64) Status {
	handler.channel, handler.address, handler.length = device, virtual, address
	return StatusOK
}

func (handler *testHandler) LogBounds(device uint64, address uint64) Status {
	handler.channel, handler.address = device, address
	return StatusOK
}

func (handler *testHandler) ConsoleInterrupt(device uint64) Status {
	handler.channel = device
	return StatusNotFound
}

func (handler *testHandler) ConsoleInfo(device uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = device, address, length
	return StatusOK
}

func (handler *testHandler) ValidationFault(device uint64) Status {
	handler.channel = device
	return StatusUnsupported
}

func (handler *testHandler) SystemPowerOff(device uint64) Status {
	handler.channel = device
	return StatusPermissionDenied
}

func (handler *testHandler) PowerOffReady(device uint64) Status {
	handler.channel = device
	return StatusPermissionDenied
}

func (handler *testHandler) ExecutableCreate(device uint64, size uint64, address uint64) Status {
	handler.channel, handler.length, handler.address = device, size, address
	return StatusOK
}

func (handler *testHandler) ExecutableWrite(executable uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = executable, address, length
	return StatusOK
}

func (handler *testHandler) ExecutableSource(device uint64, session uint64, descriptor uint64) Status {
	handler.channel, handler.length, handler.address = device, session, descriptor
	return StatusOK
}

func (handler *testHandler) VMMap(descriptor uint64, protection uint64, flags uint64) Status {
	handler.address, handler.length, handler.channel = descriptor, protection, flags
	return StatusOK
}

func (handler *testHandler) VMUnmap(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) VMProtect(address uint64, length uint64, protection uint64) Status {
	handler.address, handler.length, handler.channel = address, length, protection
	return StatusOK
}

func (handler *testHandler) VMDiscard(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) VMQuery(address uint64, record uint64) Status {
	handler.address, handler.length = address, record
	return StatusOK
}

func (handler *testHandler) MemoryCreate(pages uint64, address uint64) Status {
	handler.length, handler.address = pages, address
	return StatusOK
}

func (handler *testHandler) HandleDuplicate(id uint64, rights uint64, address uint64) Status {
	handler.channel, handler.length, handler.address = id, rights, address
	return StatusOK
}

func (handler *testHandler) ClockReadNanos(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func (handler *testHandler) ClockNow(clock uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = clock, address, length
	return StatusOK
}

func (handler *testHandler) ClockSet(authority uint64, address uint64, length uint64) Status {
	handler.channel, handler.address, handler.length = authority, address, length
	return StatusPermissionDenied
}

func (handler *testHandler) ThreadCreate(entry uint64, argument uint64, address uint64) Status {
	handler.channel, handler.length, handler.address = entry, argument, address
	return StatusOK
}

func (handler *testHandler) ThreadExit(code uint64) Status {
	handler.exitCode = code
	return StatusOK
}

func (handler *testHandler) ThreadJoin(thread uint64, flags uint64, address uint64) (Status, bool) {
	handler.channel, handler.length, handler.address = thread, flags, address
	return StatusOK, flags == 0
}

func (handler *testHandler) ThreadSelf(address uint64) Status {
	handler.address = address
	return StatusOK
}

func (handler *testHandler) ThreadYield() Status {
	handler.called = true
	return StatusOK
}

func (handler *testHandler) ThreadSuspend(address uint64, capacity uint64, count uint64) Status {
	handler.address, handler.length, handler.channel = address, capacity, count
	return StatusOK
}

func (handler *testHandler) ThreadSetBase(base uint64) Status {
	handler.address = base
	return StatusOK
}

func (handler *testHandler) ThreadPreempt(entry uint64, ticks uint64) Status {
	handler.channel, handler.length = entry, ticks
	return StatusOK
}

func (handler *testHandler) ThreadPreemptTake(address uint64) Status {
	handler.address = address
	return StatusOK
}

func (handler *testHandler) ThreadWake(thread uint64) Status {
	handler.channel = thread
	return StatusOK
}

func (handler *testHandler) ThreadWakeTake() Status {
	handler.called = true
	return StatusNotFound
}

func (handler *testHandler) ThreadResume() Status {
	handler.called = true
	return StatusOK
}

func (handler *testHandler) Exit(code uint64) Status {
	handler.exitCode = code
	return StatusOK
}

func TestDispatchExit(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationExit, Arg0: 7}, handler)
	if result.Status != StatusOK || result.Action != ActionExit || handler.exitCode != 7 {
		t.Fatalf("unexpected exit result: %#v, handler: %#v", result, handler)
	}
}

// The retired DebugWrite number reaches no handler and is refused whatever its
// arguments, so no build carries an identity-authorized console path.
func TestDispatchRefusesRetiredDebugWrite(t *testing.T) {
	handler := &testHandler{}
	for _, request := range []Request{
		{Operation: OperationRetiredDebugWrite, Arg0: 0x1234, Arg1: 32},
		{Operation: OperationRetiredDebugWrite},
		{Operation: OperationRetiredDebugWrite, Arg0: 0x1234, Arg1: MaxConsoleWrite + 1},
	} {
		if result := Dispatch(request, handler); result.Status != StatusUnsupported || result.Action != ActionReturn {
			t.Fatalf("retired debug write = %#v", result)
		}
	}
	if *handler != (testHandler{}) {
		t.Fatalf("retired debug write reached the handler: %#v", handler)
	}
}

func TestDispatchRejectsOversizedConsoleWrite(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationConsoleWrite, Arg0: 0x100000001, Arg1: 0x1234, Arg2: MaxConsoleWrite + 1}, handler)
	if result.Status != StatusInvalidArgument || handler.length != 0 {
		t.Fatalf("oversized request reached handler: %#v, handler: %#v", result, handler)
	}
}

func TestDispatchRejectsUnknownOperation(t *testing.T) {
	result := Dispatch(Request{Operation: 0xffff}, &testHandler{})
	if result.Status != StatusUnsupported || result.Action != ActionReturn {
		t.Fatalf("unexpected unknown-operation result: %#v", result)
	}
}

func TestDispatchIPC(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationIPCSend, Arg0: 9, Arg1: 0x2000, Arg2: 64}, handler)
	if result.Status != StatusOK || handler.channel != 9 || handler.address != 0x2000 || handler.length != 64 {
		t.Fatalf("unexpected send: %#v, handler: %#v", result, handler)
	}
	result = Dispatch(Request{Operation: OperationIPCReceive, Arg0: 10, Arg1: 0x3000, Arg2: 128}, handler)
	if result.Status != StatusOK || handler.channel != 10 {
		t.Fatalf("unexpected receive: %#v, handler: %#v", result, handler)
	}
}

func TestDispatchConsoleWrite(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationConsoleWrite, Arg0: 12, Arg1: 0x8000, Arg2: 15}, handler)
	if result.Status != StatusOK || handler.channel != 12 || handler.address != 0x8000 || handler.length != 15 {
		t.Fatalf("unexpected console write: %#v, handler: %#v", result, handler)
	}
}

func TestDispatchConsoleReadAndClock(t *testing.T) {
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationConsoleRead, Arg0: 1, Arg1: 0x8000, Arg2: 1}, handler); result.Status != StatusOK || !handler.called {
		t.Fatalf("console read = %#v", result)
	}
	handler.called = false
	if result := Dispatch(Request{Operation: OperationClockRead, Arg0: 0x9000, Arg1: 8}, handler); result.Status != StatusOK || !handler.called {
		t.Fatalf("clock read = %#v", result)
	}
	if result := Dispatch(Request{Operation: OperationConsoleRead, Arg2: 2}, handler); result.Status != StatusInvalidArgument {
		t.Fatalf("oversized console read = %#v", result)
	}
}

func TestDispatchRejectsOversizedIPCBuffer(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationIPCSend, Arg2: MaxIPCMessage + 1}, handler)
	if result.Status != StatusInvalidArgument || handler.length != 0 {
		t.Fatalf("oversized IPC request reached handler: %#v", result)
	}
}

func TestDispatchStartupAndHandleOperations(t *testing.T) {
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationStartupRead, Arg0: 0x4000, Arg1: 32}, handler); result.Status != StatusOK || handler.address != 0x4000 || handler.length != 32 {
		t.Fatalf("unexpected startup read: %#v, handler: %#v", result, handler)
	}
	if result := Dispatch(Request{Operation: OperationChannelCreate, Arg0: 0x5000, Arg1: 16}, handler); result.Status != StatusOK || handler.address != 0x5000 || handler.length != 16 {
		t.Fatalf("unexpected channel create: %#v, handler: %#v", result, handler)
	}
	if result := Dispatch(Request{Operation: OperationHandleClose, Arg0: 17}, handler); result.Status != StatusOK || handler.channel != 17 {
		t.Fatalf("unexpected handle close: %#v, handler: %#v", result, handler)
	}
}

func TestDispatchSystemInfo(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationSystemInfo, Arg0: 0x6000, Arg1: SystemInfoSize}, handler)
	if result.Status != StatusOK || handler.address != 0x6000 || handler.length != SystemInfoSize {
		t.Fatalf("system info = %#v, handler: %#v", result, handler)
	}
	if result = Dispatch(Request{Operation: OperationSystemInfo, Arg1: SystemInfoSize - 1}, handler); result.Status != StatusInvalidArgument {
		t.Fatalf("short system info = %#v", result)
	}
}

func TestDispatchCPUInformation(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationCPUInfo, Arg0: 0x7000, Arg1: CPUInfoSize}, handler)
	if result.Status != StatusOK || handler.address != 0x7000 || handler.length != CPUInfoSize {
		t.Fatalf("CPU info = %#v, handler: %#v", result, handler)
	}
	if result = Dispatch(Request{Operation: OperationCPUInfo, Arg1: CPUInfoSize - 1}, handler); result.Status != StatusInvalidArgument {
		t.Fatalf("short CPU info = %#v", result)
	}
	result = Dispatch(Request{Operation: OperationCPUEntry, Arg0: 3, Arg1: 0x8000}, handler)
	if result.Status != StatusOK || handler.length != 3 || handler.address != 0x8000 {
		t.Fatalf("CPU entry = %#v, handler: %#v", result, handler)
	}
	result = Dispatch(Request{Operation: OperationCPUCacheInfo, Arg0: 2, Arg1: 0x9000}, handler)
	if result.Status != StatusOK || handler.length != 2 || handler.address != 0x9000 {
		t.Fatalf("CPU cache = %#v, handler: %#v", result, handler)
	}
}

func TestDispatchStreamOperations(t *testing.T) {
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationStreamCreate, Arg0: 0x7000, Arg1: 16}, handler); result.Status != StatusOK {
		t.Fatalf("stream create = %#v", result)
	}
	if result := Dispatch(Request{Operation: OperationStreamWrite, Arg0: 3, Arg1: 0x8000, Arg2: StreamDescriptorSize}, handler); result.Status != StatusOK {
		t.Fatalf("stream write = %#v", result)
	}
	if result := Dispatch(Request{Operation: OperationStreamRead, Arg0: 4, Arg1: 0x9000, Arg2: StreamDescriptorSize}, handler); result.Status != StatusOK {
		t.Fatalf("stream read = %#v", result)
	}
	if result := Dispatch(Request{Operation: OperationStreamRead, Arg2: StreamDescriptorSize - 1}, handler); result.Status != StatusInvalidArgument {
		t.Fatalf("short stream descriptor = %#v", result)
	}
}

func TestDispatchDiagnosticsPassesDeviceHandle(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationDiagnostics, Arg0: 7, Arg1: 0x40001000}, handler)
	if result.Status != StatusUnsupported || handler.channel != 7 || handler.address != 0x40001000 || result.Action != ActionReturn {
		t.Fatalf("result %+v, device %d", result, handler.channel)
	}
}

func TestDispatchClockReadNanos(t *testing.T) {
	if OperationClockReadNanos != 24 {
		t.Fatalf("clock nanoseconds operation is %d, want 24", OperationClockReadNanos)
	}
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationClockReadNanos, Arg0: 0xa000, Arg1: 8}, handler)
	if result.Status != StatusOK || result.Action != ActionReturn || handler.address != 0xa000 || handler.length != 8 {
		t.Fatalf("clock nanoseconds = %#v, handler: %#v", result, handler)
	}
	for _, length := range []uint64{0, 4, 16} {
		handler = &testHandler{}
		if result := Dispatch(Request{Operation: OperationClockReadNanos, Arg0: 0xa000, Arg1: length}, handler); result.Status != StatusInvalidArgument || handler.length != 0 {
			t.Fatalf("length %d reached handler: %#v", length, result)
		}
	}
}

func TestDispatchLogOperations(t *testing.T) {
	if OperationLogWrite != 25 || OperationLogRead != 26 {
		t.Fatalf("log operations are %d and %d", OperationLogWrite, OperationLogRead)
	}
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationLogWrite, Arg0: 2, Arg1: 0xa000, Arg2: 40}, handler); result.Status != StatusOK || handler.length != 40 {
		t.Fatalf("log write %+v", result)
	}
	for _, length := range []uint64{0, MaxLogRecord + 1} {
		if result := Dispatch(Request{Operation: OperationLogWrite, Arg0: 2, Arg1: 0xa000, Arg2: length}, &testHandler{}); result.Status != StatusInvalidArgument {
			t.Fatalf("log write length %d accepted", length)
		}
	}
	if result := Dispatch(Request{Operation: OperationLogRead, Arg0: 2, Arg1: 3, Arg2: 0xb000}, handler); result.Status != StatusOK || handler.length != 3 || handler.address != 0xb000 {
		t.Fatalf("log read %+v", result)
	}
}

func TestDispatchSystemInformationOperations(t *testing.T) {
	if OperationHandleInfo != 27 || OperationProcessInfo != 28 || OperationSystemReset != 29 {
		t.Fatalf("operations %d %d %d", OperationHandleInfo, OperationProcessInfo, OperationSystemReset)
	}
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationHandleInfo, Arg0: 3, Arg1: 0xa000}, handler); result.Status != StatusOK || handler.length != 3 {
		t.Fatalf("handle info %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationProcessInfo, Arg0: 5, Arg1: 0xb000}, handler); result.Status != StatusOK || handler.address != 0xb000 {
		t.Fatalf("process info %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationSystemReset, Arg0: 18}, handler); result.Status != StatusPermissionDenied || handler.channel != 18 || result.Action != ActionReturn {
		t.Fatalf("system reset %+v", result)
	}
}

func TestDispatchMemoryMap(t *testing.T) {
	if OperationMemoryMap != 30 {
		t.Fatalf("operation %d", OperationMemoryMap)
	}
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationMemoryMap, Arg0: 20, Arg1: 0x40180000, Arg2: MemoryMapWritable}, handler); result.Status != StatusOK || handler.channel != 20 || handler.address != 0x40180000 || handler.length != MemoryMapWritable {
		t.Fatalf("memory map %+v %+v", result, handler)
	}
	handler = &testHandler{}
	if result := Dispatch(Request{Operation: OperationMemoryMap, Arg0: 20, Arg1: 0x40180000, Arg2: 4}, handler); result.Status != StatusInvalidArgument || handler.channel != 0 {
		t.Fatalf("unknown memory map flag accepted: %+v", result)
	}
}

func TestDispatchProcessKill(t *testing.T) {
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationProcessKill, Arg0: 0x100000009}, handler); result.Status != StatusOK || handler.channel != 0x100000009 {
		t.Fatalf("process kill = %#v, handler %#v", result, handler)
	}
}

func TestDispatchImageMap(t *testing.T) {
	if OperationImageMap != 32 {
		t.Fatalf("operation %d", OperationImageMap)
	}
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationImageMap, Arg0: 7, Arg1: 0x40200000, Arg2: 0x40004000}, handler)
	if result.Status != StatusOK || result.Action != ActionReturn || handler.channel != 7 || handler.address != 0x40200000 || handler.length != 0x40004000 {
		t.Fatalf("image map %+v %+v", result, handler)
	}

}

func TestDispatchLogBounds(t *testing.T) {
	if OperationLogBounds != 33 {
		t.Fatalf("operation %d", OperationLogBounds)
	}
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationLogBounds, Arg0: 2, Arg1: 0x40004000}, handler)
	if result.Status != StatusOK || result.Action != ActionReturn || handler.channel != 2 || handler.address != 0x40004000 {
		t.Fatalf("log bounds %+v %+v", result, handler)
	}
}

func TestDispatchConsoleInterrupt(t *testing.T) {
	if OperationConsoleInterrupt != 34 {
		t.Fatalf("operation %d", OperationConsoleInterrupt)
	}
	handler := &testHandler{}
	// No interrupt pending is a status, not an error: a shell asks often.
	if result := Dispatch(Request{Operation: OperationConsoleInterrupt, Arg0: 2}, handler); result.Status != StatusNotFound || handler.channel != 2 {
		t.Fatalf("console interrupt %+v %+v", result, handler)
	}
}

func TestDispatchValidationFault(t *testing.T) {
	if OperationValidationFault != 35 {
		t.Fatalf("operation %d", OperationValidationFault)
	}
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationValidationFault, Arg0: 7}, handler); result.Status != StatusUnsupported || handler.channel != 7 {
		t.Fatalf("validation fault %+v %+v", result, handler)
	}
}

func TestDispatchSystemPowerOff(t *testing.T) {
	if OperationSystemPowerOff != 36 {
		t.Fatalf("operation %d", OperationSystemPowerOff)
	}
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationSystemPowerOff, Arg0: 18}, handler); result.Status != StatusPermissionDenied || handler.channel != 18 || result.Action != ActionReturn {
		t.Fatalf("power off %+v %+v", result, handler)
	}
}

func TestDispatchPowerOffReady(t *testing.T) {
	if OperationPowerOffReady != 57 {
		t.Fatalf("operation %d", OperationPowerOffReady)
	}
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationPowerOffReady, Arg0: 18}, handler); result.Status != StatusPermissionDenied || handler.channel != 18 {
		t.Fatalf("power-off readiness %+v %+v", result, handler)
	}
}

func TestDispatchExecutableCalls(t *testing.T) {
	if OperationExecutableCreate != 37 || OperationExecutableWrite != 38 {
		t.Fatalf("operations %d %d", OperationExecutableCreate, OperationExecutableWrite)
	}
	if OperationVMMap != 39 || OperationVMQuery != 43 || OperationMemoryCreate != 44 || OperationHandleDuplicate != 45 || OperationClockNow != 46 || OperationClockSet != 47 {
		t.Fatalf("operations %d %d %d %d", OperationVMMap, OperationVMQuery, OperationMemoryCreate, OperationHandleDuplicate)
	}
	if OperationExecutableSource != 48 {
		t.Fatalf("source operation %d", OperationExecutableSource)
	}
	if OperationConsoleInfo != 49 {
		t.Fatalf("console info operation %d", OperationConsoleInfo)
	}
	if OperationCPUInfo != 50 || OperationCPUEntry != 51 || OperationCPUCacheInfo != 52 {
		t.Fatalf("CPU operations %d %d %d", OperationCPUInfo, OperationCPUEntry, OperationCPUCacheInfo)
	}
	if OperationCPURuntime != 53 || OperationTaskAffinity != 54 {
		t.Fatalf("runtime and affinity operations %d %d", OperationCPURuntime, OperationTaskAffinity)
	}
	if OperationIRQTake != 55 {
		t.Fatalf("IRQ take operation %d", OperationIRQTake)
	}
	irqHandler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationIRQTake, Arg0: 0x10000001d}, irqHandler); result.Status != StatusOK || irqHandler.channel != 0x10000001d {
		t.Fatalf("IRQ take dispatch %+v %#x", result, irqHandler.channel)
	}
	if OperationRandomRead != 56 {
		t.Fatalf("random read operation %d", OperationRandomRead)
	}
	randomHandler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationRandomRead, Arg0: 0x3000, Arg1: MaxRandomRead}, randomHandler); result.Status != StatusOK ||
		randomHandler.address != 0x3000 || randomHandler.length != MaxRandomRead {
		t.Fatalf("random read dispatch %+v %+v", result, randomHandler)
	}
	for _, length := range []uint64{0, MaxRandomRead + 1} {
		randomHandler = &testHandler{}
		if result := Dispatch(Request{Operation: OperationRandomRead, Arg1: length}, randomHandler); result.Status != StatusInvalidArgument || *randomHandler != (testHandler{}) {
			t.Fatalf("random read of %d bytes reached the handler", length)
		}
	}
	if result := Dispatch(Request{Operation: OperationThreadWakeTake + 1}, &testHandler{}); result.Status != StatusUnsupported {
		t.Fatalf("unknown operation accepted: %+v", result)
	}
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationExecutableCreate, Arg0: 0x100000017, Arg1: 4096, Arg2: 0x2000}, handler)
	if result.Status != StatusOK || handler.channel != 0x100000017 || handler.length != 4096 || handler.address != 0x2000 {
		t.Fatalf("create: %#v, handler: %#v", result, handler)
	}
	handler = &testHandler{}
	result = Dispatch(Request{Operation: OperationExecutableWrite, Arg0: 5, Arg1: 0x3000, Arg2: MaxExecutableWrite}, handler)
	if result.Status != StatusOK || handler.channel != 5 || handler.address != 0x3000 || handler.length != MaxExecutableWrite {
		t.Fatalf("write: %#v, handler: %#v", result, handler)
	}
	for _, length := range []uint64{0, MaxExecutableWrite + 1} {
		handler = &testHandler{}
		result = Dispatch(Request{Operation: OperationExecutableWrite, Arg0: 5, Arg1: 0x3000, Arg2: length}, handler)
		if result.Status != StatusInvalidArgument || *handler != (testHandler{}) {
			t.Fatalf("write of %d reached the handler: %#v", length, result)
		}
	}
	handler = &testHandler{}
	result = Dispatch(Request{Operation: OperationExecutableSource, Arg0: 7, Arg1: 8, Arg2: 9}, handler)
	if result.Status != StatusOK || handler.channel != 7 || handler.length != 8 || handler.address != 9 {
		t.Fatalf("source: %#v, handler: %#v", result, handler)
	}
}

func TestDispatchConsoleInfo(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationConsoleInfo, Arg0: 2, Arg1: 0x4000, Arg2: ConsoleInfoSize}, handler)
	if result.Status != StatusOK || handler.channel != 2 || handler.address != 0x4000 || handler.length != ConsoleInfoSize {
		t.Fatalf("console info: %#v, handler: %#v", result, handler)
	}
	handler = &testHandler{}
	result = Dispatch(Request{Operation: OperationConsoleInfo, Arg0: 2, Arg1: 0x4000, Arg2: ConsoleInfoSize - 1}, handler)
	if result.Status != StatusInvalidArgument || *handler != (testHandler{}) {
		t.Fatalf("invalid console info reached handler: %#v, %#v", result, handler)
	}
}

func TestDispatchClockOperations(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationClockNow, Arg0: 2, Arg1: 0x1000, Arg2: ClockTimeSize}, handler)
	if result.Status != StatusOK || handler.channel != 2 || handler.address != 0x1000 || handler.length != ClockTimeSize {
		t.Fatalf("clock now: %#v, %#v", result, handler)
	}
	handler = &testHandler{}
	result = Dispatch(Request{Operation: OperationClockSet, Arg0: 9, Arg1: 0x2000, Arg2: ClockTimeSize}, handler)
	if result.Status != StatusPermissionDenied || handler.channel != 9 {
		t.Fatalf("clock set: %#v, %#v", result, handler)
	}
	for _, operation := range []Operation{OperationClockNow, OperationClockSet} {
		handler = &testHandler{}
		if result = Dispatch(Request{Operation: operation, Arg2: ClockTimeSize - 1}, handler); result.Status != StatusInvalidArgument || *handler != (testHandler{}) {
			t.Fatalf("invalid clock record reached handler: %#v, %#v", result, handler)
		}
	}
}

func TestVMCallsValidateFlags(t *testing.T) {
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationVMMap, Arg0: 0x1000, Arg1: VMRead | VMWrite, Arg2: VMMapFixed}, handler); result.Status != StatusOK || handler.address != 0x1000 || handler.length != VMRead|VMWrite || handler.channel != VMMapFixed {
		t.Fatalf("map: %+v", result)
	}
	for _, request := range []Request{
		{Operation: OperationVMMap, Arg1: 8},
		{Operation: OperationVMMap, Arg2: 2},
		{Operation: OperationVMProtect, Arg2: 16},
		{Operation: OperationMemoryMap, Arg2: 4},
	} {
		if result := Dispatch(request, handler); result.Status != StatusInvalidArgument {
			t.Fatalf("%+v accepted: %v", request, result.Status)
		}
	}
	for _, operation := range []Operation{OperationVMUnmap, OperationVMProtect, OperationVMDiscard, OperationVMQuery} {
		if result := Dispatch(Request{Operation: operation, Arg0: 0x2000, Arg1: 0x3000}, handler); result.Status != StatusOK || handler.address != 0x2000 {
			t.Fatalf("operation %d: %+v", operation, result)
		}
	}
}

func (handler *testHandler) LogReadSequence(device, cursor, address uint64) Status {
	handler.channel, handler.length, handler.address = device, cursor, address
	return StatusOK
}

func TestLogSequenceDispatchKeepsOldOperations(t *testing.T) {
	handler := &testHandler{}
	if OperationLogReadSequence != 58 || OperationLogRead != 26 || OperationLogBounds != 33 {
		t.Fatal("log operation ABI changed")
	}
	result := Dispatch(Request{Operation: OperationLogReadSequence, Arg0: 9, Arg1: 12345, Arg2: 0x3000}, handler)
	if result.Status != StatusOK || handler.channel != 9 || handler.length != 12345 || handler.address != 0x3000 {
		t.Fatalf("sequence dispatch %+v %+v", result, handler)
	}
	if LogReadMax != 64 || LogReadHeader != 32 || LogReadRecordSize != 208 || LogReadSize != 13344 {
		t.Fatal("structured snapshot wire layout changed")
	}
}

// TestThreadOperationsAreAppended pins the thread call numbers after every
// earlier one, so no existing number moved (ADR 0203).
func TestThreadOperationsAreAppended(t *testing.T) {
	if OperationThreadCreate != 59 || OperationThreadResume != 65 {
		t.Fatalf("thread operations %d..%d", OperationThreadCreate, OperationThreadResume)
	}
}

func TestDispatchThreadCalls(t *testing.T) {
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationThreadCreate, Arg0: 0x40002000, Arg1: 7, Arg2: 0x5000}, handler); result.Status != StatusOK || result.Action != ActionReturn ||
		handler.channel != 0x40002000 || handler.length != 7 || handler.address != 0x5000 {
		t.Fatalf("create %+v %+v", result, handler)
	}
	if result := Dispatch(Request{Operation: OperationThreadExit, Arg0: 3}, handler); result.Action != ActionExit || handler.exitCode != 3 {
		t.Fatalf("exit %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationThreadJoin, Arg0: 9, Arg2: 0x6000}, handler); result.Action != ActionBlock {
		t.Fatalf("a waiting join %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationThreadJoin, Arg0: 9, Arg1: JoinPoll, Arg2: 0x6000}, handler); result.Action != ActionReturn {
		t.Fatalf("a polling join %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationThreadJoin, Arg0: 9, Arg1: 2}, handler); result.Status != StatusInvalidArgument {
		t.Fatalf("unknown join flags %+v", result)
	}
	for _, capacity := range []uint64{0, MaxThreadSnapshots + 1} {
		if result := Dispatch(Request{Operation: OperationThreadSuspend, Arg0: 0x7000, Arg1: capacity}, handler); result.Status != StatusInvalidArgument {
			t.Fatalf("suspend capacity %d: %+v", capacity, result)
		}
	}
	if result := Dispatch(Request{Operation: OperationThreadSuspend, Arg0: 0x7000, Arg1: MaxThreadSnapshots, Arg2: 0x8000}, handler); result.Status != StatusOK || handler.length != MaxThreadSnapshots {
		t.Fatalf("suspend %+v", result)
	}
}

// TestPreemptionOperationsAreAppended pins the numbers of the calls the
// goroutine runtime added after the thread calls.
func TestPreemptionOperationsAreAppended(t *testing.T) {
	if OperationThreadSetBase != 66 || OperationThreadPreempt != 67 || OperationThreadPreemptTake != 68 ||
		OperationThreadWake != 69 || OperationThreadWakeTake != 70 {
		t.Fatalf("goroutine operations %d..%d", OperationThreadSetBase, OperationThreadWakeTake)
	}
}

func TestDispatchPreemptionCalls(t *testing.T) {
	handler := &testHandler{}
	if result := Dispatch(Request{Operation: OperationThreadSetBase, Arg0: 0x7000}, handler); result.Status != StatusOK || handler.address != 0x7000 {
		t.Fatalf("set base %+v %#x", result, handler.address)
	}
	if result := Dispatch(Request{Operation: OperationThreadPreempt, Arg0: 0x40002000, Arg1: 2}, handler); result.Status != StatusOK || handler.channel != 0x40002000 || handler.length != 2 {
		t.Fatalf("preempt %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationThreadPreempt}, handler); result.Status != StatusOK {
		t.Fatalf("preempt off %+v", result)
	}
	for _, bad := range []Request{
		{Operation: OperationThreadPreempt, Arg0: 0x40002000},
		{Operation: OperationThreadPreempt, Arg1: 2},
		{Operation: OperationThreadPreempt, Arg0: 0x40002000, Arg1: MaxPreemptTicks + 1},
	} {
		if result := Dispatch(bad, &testHandler{}); result.Status != StatusInvalidArgument {
			t.Fatalf("preempt %+v accepted: %+v", bad, result)
		}
	}
	if result := Dispatch(Request{Operation: OperationThreadPreemptTake, Arg0: 0x8000}, handler); result.Status != StatusOK || handler.address != 0x8000 {
		t.Fatalf("take %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationThreadWake, Arg0: 0x200000003}, handler); result.Status != StatusOK || handler.channel != 0x200000003 {
		t.Fatalf("wake %+v", result)
	}
	if result := Dispatch(Request{Operation: OperationThreadWakeTake}, handler); result.Status != StatusNotFound || !handler.called {
		t.Fatalf("wake take %+v", result)
	}
}

func TestThreadStackLayout(t *testing.T) {
	if ThreadAreaTop%4096 != 0 || ThreadStride%4096 != 0 || ThreadSnapshotSize%8 != 0 {
		t.Fatal("thread layout is not page or word aligned")
	}
	for slot := uint64(1); slot < ThreadSlots; slot++ {
		top := ThreadStackTop(slot)
		base := top - ThreadStackSize
		for _, address := range []uint64{top - 1, base, base + 4096} {
			if ThreadSlotOf(address) != slot {
				t.Fatalf("slot %d: %#x maps to %d", slot, address, ThreadSlotOf(address))
			}
		}
		// The guard below a stack belongs to its slot's stride.
		if guard := base - 1; ThreadSlotOf(guard) != slot {
			t.Fatalf("slot %d: its guard maps to %d", slot, ThreadSlotOf(guard))
		}
	}
	if ThreadSlotOf(ThreadAreaTop) != 0 || ThreadSlotOf(0x00007ffffefff000) != 0 {
		t.Fatal("the main stack is not slot 0")
	}
	if ThreadSlotOf(ThreadStackTop(ThreadSlots-1)-ThreadStride-1) != ThreadSlots || ThreadSlotOf(0x1000) != ThreadSlots {
		t.Fatal("an address below the thread area maps to a slot")
	}
}
