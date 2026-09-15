package kcall

import "testing"

type testHandler struct {
	exitCode uint64
	address  uint64
	length   uint64
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
