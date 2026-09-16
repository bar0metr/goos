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
