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

func (handler *testHandler) Exit(code uint64) Status {
	handler.exitCode = code
	return StatusOK
}

func (handler *testHandler) DebugWrite(address uint64, length uint64) Status {
	handler.address, handler.length = address, length
	return StatusOK
}

func TestDispatchExit(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationExit, Arg0: 7}, handler)
	if result.Status != StatusOK || result.Action != ActionExit || handler.exitCode != 7 {
		t.Fatalf("unexpected exit result: %#v, handler: %#v", result, handler)
	}
}

func TestDispatchDebugWrite(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationDebugWrite, Arg0: 0x1234, Arg1: 32}, handler)
	if result.Status != StatusOK || result.Action != ActionReturn || handler.address != 0x1234 || handler.length != 32 {
		t.Fatalf("unexpected write result: %#v, handler: %#v", result, handler)
	}
}

func TestDispatchRejectsOversizedWrite(t *testing.T) {
	handler := &testHandler{}
	result := Dispatch(Request{Operation: OperationDebugWrite, Arg0: 0x1234, Arg1: MaxDebugWrite + 1}, handler)
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
	if result := Dispatch(Request{Operation: OperationStartupRead, Arg0: 0x4000, Arg1: 128}, handler); result.Status != StatusOK || handler.address != 0x4000 || handler.length != 128 {
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
