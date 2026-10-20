package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"testing"
)

func readMessage(t testing.TB, request vfs.Request) ipc.Message {
	t.Helper()
	message, ok := vfs.EncodeRequest(request, 1)
	if !ok {
		t.Fatal("encode")
	}
	if request.Operation == vfs.OperationReadInto {
		message.HandleCount, message.Handles[0] = 1, handle.ID(7)
	}
	return message
}

func TestParallelReadOnlyAcceptsCanonicalForms(t *testing.T) {
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationRead, Handle: 1, Size: 4},
		{Operation: vfs.OperationReadInto, Handle: 1, Offset: 7, Size: 4},
	} {
		if _, valid := parallelRead(readMessage(t, request)); !valid {
			t.Fatalf("canonical read refused: %+v", request)
		}
	}
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationRead, Path: []byte("/tmp/.validation-vfs-stall")},
		{Operation: vfs.OperationRead, Target: []byte("/unused")},
		{Operation: vfs.OperationRead, Data: []byte("unused")},
		{Operation: vfs.OperationRead, Rights: vfs.Read},
		{Operation: vfs.OperationRead, Offset: 1},
		{Operation: vfs.OperationRead, MountID: 1},
		{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Read},
		{Operation: vfs.OperationStat, Handle: 1},
		{Operation: vfs.OperationReadExecutable, Handle: 1, Size: 4},
		{Operation: vfs.OperationPrepareExecutable, Handle: 1, Size: 4},
		{Operation: vfs.OperationReadMount},
	} {
		if _, valid := parallelRead(readMessage(t, request)); valid {
			t.Fatalf("noncanonical operation admitted: %+v", request)
		}
	}
	plain := readMessage(t, vfs.Request{Operation: vfs.OperationRead})
	plain.HandleCount = 1
	if _, valid := parallelRead(plain); valid {
		t.Fatal("unexpected attachment admitted")
	}
	bulk := readMessage(t, vfs.Request{Operation: vfs.OperationReadInto})
	bulk.HandleCount = 0
	if _, valid := parallelRead(bulk); valid {
		t.Fatal("missing bulk attachment admitted")
	}
	plain.ProtocolID = 0
	if _, valid := parallelRead(plain); valid {
		t.Fatal("malformed message admitted")
	}
}

func TestBulkReadBounds(t *testing.T) {
	for _, test := range []struct {
		object, offset uint64
		size           uint32
		valid          bool
	}{{8, 4, 4, true}, {8, 8, 0, true}, {8, 8, 1, false}, {maxBulkObject + 1, 0, 1, false}, {8, ^uint64(0), 2, false}} {
		end, valid := bulkReadWindow(test.object, test.offset, test.size)
		if valid != test.valid || valid && end != test.offset+uint64(test.size) {
			t.Fatalf("window %+v: end=%d valid=%t", test, end, valid)
		}
	}
}

func TestSessionGenerationAndWaitBudget(t *testing.T) {
	if generation, valid := nextSessionGeneration(0); !valid || generation != 1 {
		t.Fatal("initial generation")
	}
	if _, valid := nextSessionGeneration(^uint64(0)); valid {
		t.Fatal("session generation wrapped")
	}
	if !sameSession(7, 2, 7, 2) || sameSession(7, 1, 7, 2) || sameSession(7, 2, 8, 2) || sameSession(0, 1, 0, 1) {
		t.Fatal("stale session accepted")
	}
	if concurrentSessionLimit+2+1+1 != kcall.WaitAnyMaxObjects {
		t.Fatal("owner interests, remote waiter and poller wake exceed kernel wait budget")
	}
}

func TestCompletionWakeCoalescesOnlyAFullStream(t *testing.T) {
	if !completionWakeWritten(1, kcall.StatusOK) || !completionWakeWritten(0, kcall.StatusResourceExhausted) {
		t.Fatal("valid completion wake refused")
	}
	if completionWakeWritten(0, kcall.StatusOK) || completionWakeWritten(1, kcall.StatusResourceExhausted) || completionWakeWritten(0, kcall.StatusPeerDied) {
		t.Fatal("broken completion wake accepted")
	}
}
