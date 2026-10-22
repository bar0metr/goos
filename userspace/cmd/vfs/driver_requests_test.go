//go:build !goos_userspace || goos_goroutines

package main

import (
	"testing"
	"unsafe"

	"goos/kernel/handle"
	"goos/kernel/vfs"
	vfsservice "goos/userspace/vfs"
)

func TestDriverRequestsKeepAttachmentsAndMappingsUntilRelease(t *testing.T) {
	requests := &driverRequests{}
	request := driverRequest{ticket: 1, kind: driverRead, index: 2, session: 7, generation: 3, mapBase: 4096, mapSize: 8192}
	request.message.HandleCount, request.message.Handles[0] = 1, handle.ID(11)
	if !requests.put(request) || requests.put(request) || requests.put(driverRequest{}) {
		t.Fatal("duplicate or zero ticket admitted")
	}
	got := requests.find(1)
	if got == nil || got.mapBase != 4096 || got.mapSize != 8192 || got.message.Handles[0] != 11 || got.generation != 3 {
		t.Fatal("request ownership lost")
	}
	if requests.release(2) || !requests.release(1) || requests.release(1) || requests.find(1) != nil {
		t.Fatal("release was not ticket-specific and exactly once")
	}
	if !requests.put(driverRequest{ticket: 2, generation: 4}) || requests.release(1) {
		t.Fatal("stale ticket released reused record")
	}
}

func TestDriverRequestsAreBoundedLikePoolIngress(t *testing.T) {
	requests := &driverRequests{}
	for index := range requests {
		if !requests.put(driverRequest{ticket: uint64(index + 1)}) {
			t.Fatal("capacity smaller than pool")
		}
	}
	if requests.put(driverRequest{ticket: uint64(len(requests) + 1)}) {
		t.Fatal("unbounded request metadata")
	}
}

func tornBytes(length int) []byte {
	header := struct {
		data     unsafe.Pointer
		len, cap int
	}{nil, length, length}
	return *(*[]byte)(unsafe.Pointer(&header))
}

// A completion is checked against the request that holds its ticket before
// the reply copies its bytes. A session that closed and reopened leaves a
// consistent stale completion, which is valid: its reply is dropped.
func TestReadCompletionFaultChecksSessionAndBuffer(t *testing.T) {
	var inline [8]byte
	read := &driverRequest{ticket: 4, kind: driverRead, index: 1, session: 9, generation: 3, operation: vfs.OperationRead}
	into := &driverRequest{ticket: 5, kind: driverRead, index: 1, session: 9, generation: 3, operation: vfs.OperationReadInto, mapBase: 0x5000, mapSize: 4096}
	session := vfsservice.ReadSession{Owner: 2, Generation: 3}
	cases := []struct {
		name       string
		request    *driverRequest
		completion vfsservice.ReadCompletion
		current    uint64
		invalid    bool
	}{
		{"inline", read, vfsservice.ReadCompletion{Ticket: 4, Session: session, Count: 8, Data: inline[:]}, 3, false},
		{"stale session, consistent", read, vfsservice.ReadCompletion{Ticket: 4, Session: session, Count: 8, Data: inline[:]}, 4, false},
		{"failed read", read, vfsservice.ReadCompletion{Ticket: 4, Session: session, Status: vfs.StatusBusy}, 3, false},
		{"bulk", into, vfsservice.ReadCompletion{Ticket: 5, Session: session, Count: 4096}, 3, false},
		{"torn data", read, vfsservice.ReadCompletion{Ticket: 4, Session: session, Count: 8, Data: tornBytes(8)}, 3, true},
		{"count past data", read, vfsservice.ReadCompletion{Ticket: 4, Session: session, Count: 9, Data: inline[:]}, 3, true},
		{"other generation", read, vfsservice.ReadCompletion{Ticket: 4, Session: vfsservice.ReadSession{Owner: 2, Generation: 4}, Count: 8, Data: inline[:]}, 4, true},
		{"other owner", read, vfsservice.ReadCompletion{Ticket: 4, Session: vfsservice.ReadSession{Owner: 3, Generation: 3}}, 3, true},
		{"barrier for a read", read, vfsservice.ReadCompletion{Ticket: 4, Session: session, Barrier: true}, 3, true},
		{"bulk past mapping", into, vfsservice.ReadCompletion{Ticket: 5, Session: session, Count: 4097}, 3, true},
	}
	for _, test := range cases {
		fault, invalid := readCompletionFault(test.request, test.completion, 2, test.current)
		if invalid != test.invalid {
			t.Errorf("%s: invalid = %v", test.name, invalid)
			continue
		}
		if invalid && (fault.Op != "reply" || fault.Current != test.current || fault.Ticket != test.completion.Ticket ||
			fault.Session != test.completion.Session || fault.Worker != vfsservice.CopyWorkerOwner) {
			t.Errorf("%s: fault = %+v", test.name, fault)
		}
	}
}
