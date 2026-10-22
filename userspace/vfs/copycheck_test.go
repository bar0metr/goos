//go:build !goos_userspace || goos_goroutines

package vfsservice

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"goos/internal/validation"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
)

// tornSlice builds the header a torn or corrupted read leaves: no array and
// a nonzero length. Safe Go cannot produce it.
func tornSlice(length int) []byte {
	header := struct {
		data     unsafe.Pointer
		len, cap int
	}{nil, length, length}
	return *(*[]byte)(unsafe.Pointer(&header))
}

func TestCopyChecksRejectOnlyArraylessBytes(t *testing.T) {
	var buffer [8]byte
	torn := tornSlice(4096)
	cases := []struct {
		name     string
		dst, src []byte
		valid    bool
	}{
		{"both arrays", buffer[:], buffer[4:], true},
		{"nil source moves nothing", buffer[:], nil, true},
		{"empty destination moves nothing", buffer[:0], torn, true},
		{"torn source", buffer[:], torn, false},
		{"torn destination", torn, buffer[:], false},
	}
	for _, test := range cases {
		if got := copyValid(test.dst, test.src); got != test.valid {
			t.Errorf("%s: copyValid = %v", test.name, got)
		}
	}
	if bufferValid(torn) || !bufferValid(nil) || !bufferValid(buffer[:]) {
		t.Fatal("bufferValid")
	}
}

func TestCopyFaultMarkerIsOneStructuredLine(t *testing.T) {
	fault := CopyFault{Op: "execute", Src: 0, Dst: 0x7f00, Len: 4096, Session: ReadSession{Owner: 3, Generation: 5},
		Current: 6, Ticket: 12, State: "running", Worker: 1}
	want := "VFS_COPY_INVALID op=execute src=0x0 dst=0x7f00 len=4096 session=3 gen=5 cur_gen=6 plan=12 state=running worker=1\n"
	if got := string(fault.AppendMarker(nil)); got != want {
		t.Fatalf("marker = %q", got)
	}
	fault.Worker, fault.Len = CopyWorkerOwner, -1
	if got := string(fault.AppendMarker(nil)); got[len(got)-14:] != " worker=owner\n" || !strings.Contains(got, " len=-1 ") {
		t.Fatalf("owner marker = %q", got)
	}
}

type copyFaultStop struct{ fault CopyFault }

// The owner refuses to hand a worker a target without an array, before the
// worker's backend copy would fault in memmove.
func TestReadPoolCopyCheckStopsTornTargetBeforeDispatch(t *testing.T) {
	if !validation.Enabled {
		t.Skip("validation checks")
	}
	service, _, first, _ := poolFixture(t, 2)
	service.SetCopyFault(func(fault CopyFault) { panic(copyFaultStop{fault}) })
	pool, ok := NewReadPool(service, 2)
	if !ok {
		t.Fatal("pool")
	}
	session := ReadSession{Owner: 1, Generation: 4}
	var fault CopyFault
	func() {
		defer func() {
			stop, stopped := recover().(copyFaultStop)
			if !stopped {
				t.Fatal("torn target dispatched")
			}
			fault = stop.fault
		}()
		pool.AdmitReadInto(session, 9, first, tornSlice(64))
	}()
	if fault.Op != "dispatch" || fault.Dst != 0 || fault.Len != 64 || fault.Session != session ||
		fault.Ticket == 0 || fault.State != "queued" || fault.Worker != CopyWorkerOwner {
		t.Fatalf("fault = %+v", fault)
	}
	// Repair the white-box state so the pool drains normally.
	pool.copyFault = nil
	pool.slots[pool.order[0]].target = nil
	pool.schedule()
	completed := nextPool(t, pool)
	if completed.Ticket != fault.Ticket || completed.Count != 0 || !pool.Retire(completed.Ticket) || !pool.Close() {
		t.Fatalf("drain = %+v", completed)
	}
}

// A worker checks the captured target again, so a header torn after
// dispatch is reported with its worker index instead of faulting.
func TestReadPoolWorkerCopyCheckNamesWorker(t *testing.T) {
	if !validation.Enabled {
		t.Skip("validation checks")
	}
	service := startedService(t, nil)
	faults := make(chan CopyFault, 1)
	service.SetCopyFault(func(fault CopyFault) {
		faults <- fault
		runtime.Goexit()
	})
	pool, ok := NewReadPool(service, 1)
	if !ok {
		t.Fatal("pool")
	}
	session := ReadSession{Owner: 2, Generation: 7}
	pool.jobs <- readWork{slot: 3, ticket: 41, session: session, target: tornSlice(512)}
	fault := <-faults
	if fault.Op != "execute" || fault.Dst != 0 || fault.Len != 512 || fault.Session != session ||
		fault.Ticket != 41 || fault.State != "running" || fault.Worker != 0 {
		t.Fatalf("fault = %+v", fault)
	}
	if !pool.Close() {
		t.Fatal("close")
	}
}

// Delivered completions keep Data inside the slot's target; a mismatch is a
// plan-state fault, reported before the owner copies Data into a reply.
func TestReadPoolDeliveryCheckRejectsMismatchedData(t *testing.T) {
	if !validation.Enabled {
		t.Skip("validation checks")
	}
	service := startedService(t, nil)
	service.SetCopyFault(func(fault CopyFault) { panic(copyFaultStop{fault}) })
	pool, ok := NewReadPool(service, 1)
	if !ok {
		t.Fatal("pool")
	}
	session := ReadSession{Owner: 1, Generation: 2}
	ticket, status := pool.AdmitBarrier(session, 5)
	if status != vfs.StatusOK {
		t.Fatal(status)
	}
	entry := &pool.slots[pool.order[0]]
	if entry.state != readCompleted {
		t.Fatalf("barrier state = %s", entry.state)
	}
	entry.completion.Count = 3
	var fault CopyFault
	func() {
		defer func() {
			stop, stopped := recover().(copyFaultStop)
			if !stopped {
				t.Fatal("mismatched completion delivered")
			}
			fault = stop.fault
		}()
		pool.Poll()
	}()
	if fault.Op != "deliver" || fault.Len != 3 || fault.Ticket != ticket || fault.Session != session || fault.State != "completed" {
		t.Fatalf("fault = %+v", fault)
	}
	pool.copyFault = nil
	entry.completion.Count = 0
	if completion, ok := pool.Poll(); !ok || completion.Ticket != ticket || !pool.Retire(ticket) || !pool.Close() {
		t.Fatalf("drain = %+v", completion)
	}
}

func TestRemoteCopyCheckRejectsTornCacheBeforeSlicing(t *testing.T) {
	if !validation.Enabled {
		t.Skip("validation checks")
	}
	remote := &Remote{}
	remote.pages.memory = tornSlice(fsproto.CacheSize)
	remote.pages.Fill(0, 17, 0, fsproto.PageSize)
	remote.copyContext = CopyFault{Session: ReadSession{Owner: 2, Generation: 9}, Current: 9, Ticket: 31, State: "running", Worker: 1}
	remote.copyFault = func(fault CopyFault) { panic(copyFaultStop{fault}) }
	defer func() {
		stop, ok := recover().(copyFaultStop)
		if !ok {
			t.Fatal("torn cache reached copy")
		}
		fault := stop.fault
		if fault.Op != "cache" || fault.Src != 0 || fault.Ticket != 31 || fault.Session.Generation != 9 || fault.Worker != 1 {
			t.Fatalf("fault = %+v", fault)
		}
	}()
	var target [8]byte
	remote.Read(17, 0, target[:])
}
