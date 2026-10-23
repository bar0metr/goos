//go:build !goos_userspace || goos_goroutines

package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	"testing"
	"time"
)

func poolFixture(t *testing.T, workers int) (*Service, *ReadPool, vfs.Handle, vfs.Handle) {
	t.Helper()
	service := startedService(t, nil)
	if service.Dispatch(1, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/read")}).Status != vfs.StatusOK {
		t.Fatal("create")
	}
	writer := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Write})
	if writer.Status != vfs.StatusOK || service.Dispatch(1, vfs.Request{Operation: vfs.OperationWrite, Handle: writer.Handle, Data: []byte("abcdefgh")}).Status != vfs.StatusOK {
		t.Fatal("write")
	}
	service.Dispatch(1, vfs.Request{Operation: vfs.OperationClose, Handle: writer.Handle})
	first := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Read})
	second := service.Dispatch(2, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Read})
	if first.Status != vfs.StatusOK || second.Status != vfs.StatusOK {
		t.Fatal("open")
	}
	pool, ok := NewReadPool(service, workers)
	if !ok {
		t.Fatal("pool")
	}
	t.Cleanup(func() {
		if !pool.Close() {
			t.Error("pool still owns requests at cleanup")
		}
	})
	return service, pool, first.Handle, second.Handle
}

func nextPool(t *testing.T, pool *ReadPool) ReadCompletion {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if completion, ok := pool.Poll(); ok {
			return completion
		}
		select {
		case <-pool.Wake():
		case <-deadline.C:
			t.Fatal("pool completion timed out")
		}
	}
}

func TestReadPoolRetirementHoldsOwnerAndBarrier(t *testing.T) {
	service, pool, first, second := poolFixture(t, 2)
	one := ReadSession{Owner: 1, Generation: 7}
	two := ReadSession{Owner: 2, Generation: 9}
	firstTicket, status := pool.AdmitRead(one, 11, first, 2)
	if status != vfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := pool.AdmitRead(one, 12, first, 2); status != vfs.StatusBusy {
		t.Fatalf("owner reused before retirement = %d", status)
	}
	barrier, status := pool.AdmitBarrier(ReadSession{}, 13)
	if status != vfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := pool.AdmitRead(two, 14, second, 2); status != vfs.StatusOK {
		t.Fatal(status)
	}
	completed := nextPool(t, pool)
	if completed.Ticket != firstTicket || completed.Session != one || completed.Correlation != 11 || string(completed.Data) != "ab" {
		t.Fatalf("first = %+v", completed)
	}
	if _, ok := pool.Poll(); ok {
		t.Fatal("barrier passed an unretired read")
	}
	if !pool.Retire(firstTicket) {
		t.Fatal("retire read")
	}
	completed = nextPool(t, pool)
	if !completed.Barrier || completed.Ticket != barrier {
		t.Fatalf("barrier = %+v", completed)
	}
	if _, ok := pool.Poll(); ok {
		t.Fatal("read passed an unretired barrier")
	}
	if service.Dispatch(2, vfs.Request{Operation: vfs.OperationSeek, Handle: second, Offset: 4}).Status != vfs.StatusOK {
		t.Fatal("barrier seek")
	}
	if !pool.Retire(barrier) {
		t.Fatal("retire barrier")
	}
	completed = nextPool(t, pool)
	if completed.Correlation != 14 || string(completed.Data) != "ef" {
		t.Fatalf("post-barrier read planned too early: %+v", completed)
	}
	if !pool.Retire(completed.Ticket) || pool.Retire(completed.Ticket) {
		t.Fatal("retire result exactly once")
	}
	if _, status := pool.AdmitRead(one, 15, first, 2); status != vfs.StatusOK {
		t.Fatal(status)
	}
	completed = nextPool(t, pool)
	if string(completed.Data) != "cd" {
		t.Fatalf("cursor after retirement = %q", completed.Data)
	}
	pool.Retire(completed.Ticket)
}

func TestReadPoolCapacityAndSessionGeneration(t *testing.T) {
	_, pool, _, _ := poolFixture(t, 2)
	if pool.Available() != ReadQueueCapacity || pool.Busy(1) || pool.Busy(0) {
		t.Fatal("initial ingress state")
	}
	var tickets [ReadQueueCapacity]uint64
	for index := range tickets {
		ticket, status := pool.AdmitBarrier(ReadSession{Owner: vfs.Owner(index + 1), Generation: 1}, uint64(index+1))
		if status != vfs.StatusOK {
			t.Fatal(status)
		}
		tickets[index] = ticket
	}
	if pool.Available() != 0 || !pool.Busy(1) {
		t.Fatal("full ingress state")
	}
	if _, status := pool.AdmitBarrier(ReadSession{}, 100); status != vfs.StatusResourceExhausted {
		t.Fatalf("full queue = %d", status)
	}
	if pool.Close() {
		t.Fatal("closed pool while requests remain")
	}
	if pool.Retire(tickets[1]) {
		t.Fatal("retired undispatched request")
	}
	for _, ticket := range tickets {
		completion := nextPool(t, pool)
		if completion.Ticket != ticket || !pool.Retire(ticket) {
			t.Fatalf("FIFO barrier = %+v", completion)
		}
	}
	ticket, status := pool.AdmitBarrier(ReadSession{Owner: 1, Generation: 2}, 101)
	if status != vfs.StatusOK {
		t.Fatal(status)
	}
	completion := nextPool(t, pool)
	if completion.Session.Generation != 2 || pool.Retire(tickets[0]) || !pool.Retire(ticket) {
		t.Fatal("old ticket affected reused session")
	}
}

// blockedReadStore stops only file-page reads after setup. It is the actual
// RAMFS Store path, so the overlap test does not replace production Read work
// with a test-only goroutine adapter.
type blockedReadStore struct {
	pages   []*[ramfs.PageSize]byte
	hold    bool
	entered chan struct{}
	release chan struct{}
}

func (store *blockedReadStore) Allocate() (ramfs.PageRef, bool) {
	store.pages = append(store.pages, new([ramfs.PageSize]byte))
	return ramfs.PageRef(len(store.pages)), true
}
func (store *blockedReadStore) Page(ref ramfs.PageRef) *[ramfs.PageSize]byte {
	if store.hold {
		store.entered <- struct{}{}
		<-store.release
	}
	return store.pages[ref-1]
}
func (*blockedReadStore) Free(ramfs.PageRef)      {}
func (*blockedReadStore) Chunk() *ramfs.NodeChunk { return new(ramfs.NodeChunk) }

func waitReadEntry(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("independent reads did not overlap inside RAMFS")
	}
}

func blockedRAMFSFixture(t *testing.T) (*Service, *blockedReadStore, vfs.Handle, vfs.Handle) {
	t.Helper()
	store := &blockedReadStore{entered: make(chan struct{}, 2), release: make(chan struct{})}
	service := &Service{}
	if !service.ConfigureRAMFS(store, ramfs.Limits{Nodes: 16, Pages: 16}) || !service.Init() {
		t.Fatal("init")
	}
	service.Dispatch(1, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/read")})
	writer := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Write})
	if writer.Status != vfs.StatusOK || service.Dispatch(1, vfs.Request{Operation: vfs.OperationWrite, Handle: writer.Handle, Data: []byte("parallel")}).Status != vfs.StatusOK {
		t.Fatal("write")
	}
	service.Dispatch(1, vfs.Request{Operation: vfs.OperationClose, Handle: writer.Handle})
	first := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Read})
	second := service.Dispatch(2, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Read})
	if first.Status != vfs.StatusOK || second.Status != vfs.StatusOK {
		t.Fatal("open")
	}
	store.hold = true
	return service, store, first.Handle, second.Handle
}

func TestReadPoolTwoRealRAMFSReadsOverlap(t *testing.T) {
	service, store, first, second := blockedRAMFSFixture(t)
	pool, ok := NewReadPool(service, 2)
	if !ok {
		t.Fatal("pool")
	}
	defer func() {
		if !pool.Close() {
			t.Error("pool not drained")
		}
	}()
	if _, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 1}, 1, first, 4); status != vfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := pool.AdmitRead(ReadSession{Owner: 2, Generation: 1}, 2, second, 8); status != vfs.StatusOK {
		t.Fatal(status)
	}
	waitReadEntry(t, store.entered)
	waitReadEntry(t, store.entered)
	if _, ok := pool.Poll(); ok {
		t.Fatal("read completed while both workers were held")
	}
	close(store.release)
	for index := 0; index < 2; index++ {
		completion := nextPool(t, pool)
		want := "parallel"
		if completion.Session.Owner == 1 {
			want = "para"
		}
		if completion.Status != vfs.StatusOK || string(completion.Data) != want || !pool.Retire(completion.Ticket) {
			t.Fatalf("completion = %+v", completion)
		}
	}
}

func TestReadPoolReadIntoAndFailedOwnerValidation(t *testing.T) {
	_, pool, first, _ := poolFixture(t, 2)
	var target [7]byte
	if _, status := pool.AdmitReadInto(ReadSession{Owner: 2, Generation: 1}, 1, first, target[:]); status != vfs.StatusOK {
		t.Fatal(status)
	}
	failed := nextPool(t, pool)
	if failed.Status != vfs.StatusNotFound || failed.Count != 0 || len(failed.Data) != 0 || !pool.Retire(failed.Ticket) {
		t.Fatalf("wrong owner = %+v", failed)
	}
	if _, status := pool.AdmitReadInto(ReadSession{Owner: 1, Generation: 1}, 2, first, target[:]); status != vfs.StatusOK {
		t.Fatal(status)
	}
	completed := nextPool(t, pool)
	if string(target[:]) != "abcdefg" || completed.Count != 7 || completed.Status != vfs.StatusOK {
		t.Fatalf("read into = %+v target = %q", completed, target)
	}
	if !pool.Retire(completed.Ticket) || !pool.Close() {
		t.Fatal("drain")
	}
	if pool.Available() != 0 || pool.Busy(1) {
		t.Fatal("closed ingress state")
	}
	if _, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 2}, 3, first, 1); status != vfs.StatusInvalidArgument {
		t.Fatalf("closed admission = %d", status)
	}
}

type heldReadTransport struct {
	inner   Transport
	entered chan struct{}
	release chan struct{}
	fail    bool
}

func (transport *heldReadTransport) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if request.Operation == fsproto.OperationRead {
		transport.entered <- struct{}{}
		<-transport.release
		if transport.fail {
			return fsproto.Reply{}, ramfs.StatusIOError
		}
	}
	return transport.inner.Exchange(request)
}

func TestReadPoolPersistentLaneDoesNotBlockLocalReader(t *testing.T) {
	service := startedService(t, testFAT32Device())
	writePersistent(t, service, "/home/first", "first")
	writePersistent(t, service, "/home/second", "second")
	var files [3]vfs.Handle
	for index, path := range []string{"/home/first", "/home/second", "/bootstrap/motd"} {
		opened := service.Dispatch(vfs.Owner(index+1), vfs.Request{Operation: vfs.OperationOpen, Path: []byte(path), Rights: vfs.Read})
		if opened.Status != vfs.StatusOK {
			t.Fatal(opened.Status)
		}
		files[index] = opened.Handle
	}
	service.persistent.pages.ForgetAll()
	transport := &heldReadTransport{inner: service.persistent.transport, entered: make(chan struct{}, 2), release: make(chan struct{})}
	service.persistent.transport = transport
	pool, ok := NewReadPool(service, 2)
	if !ok {
		t.Fatal("pool")
	}
	defer func() {
		if !pool.Close() {
			t.Error("pool not drained")
		}
	}()
	for index, file := range files {
		if _, status := pool.AdmitRead(ReadSession{Owner: vfs.Owner(index + 1), Generation: 1}, uint64(index+1), file, 4); status != vfs.StatusOK {
			t.Fatal(status)
		}
	}
	waitReadEntry(t, transport.entered)
	local := nextPool(t, pool)
	if local.Session.Owner != 3 || local.Status != vfs.StatusOK || string(local.Data) != "Welc" {
		t.Fatalf("local reader behind held persistent read = %+v", local)
	}
	if !pool.Retire(local.Ticket) {
		t.Fatal("retire local")
	}
	select {
	case <-transport.entered:
		t.Fatal("second persistent read entered the shared Remote concurrently")
	default:
	}
	transport.release <- struct{}{}
	first := nextPool(t, pool)
	if first.Session.Owner != 1 || first.Status != vfs.StatusOK || string(first.Data) != "firs" {
		t.Fatalf("first persistent = %+v", first)
	}
	pool.Retire(first.Ticket)
	waitReadEntry(t, transport.entered)
	transport.release <- struct{}{}
	second := nextPool(t, pool)
	if second.Session.Owner != 2 || second.Status != vfs.StatusOK || string(second.Data) != "seco" || !pool.Retire(second.Ticket) {
		t.Fatalf("second persistent = %+v", second)
	}
}

func TestReadPoolBackendFailureRetiresWithoutMovingCursor(t *testing.T) {
	service := startedService(t, testFAT32Device())
	writePersistent(t, service, "/home/read", "unchanged")
	opened := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/home/read"), Rights: vfs.Read})
	if opened.Status != vfs.StatusOK {
		t.Fatal(opened.Status)
	}
	service.persistent.pages.ForgetAll()
	transport := &heldReadTransport{inner: service.persistent.transport, entered: make(chan struct{}, 1), release: make(chan struct{}, 1), fail: true}
	transport.release <- struct{}{}
	service.persistent.transport = transport
	pool, ok := NewReadPool(service, 1)
	if !ok {
		t.Fatal("pool")
	}
	if _, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 1}, 1, opened.Handle, 4); status != vfs.StatusOK {
		t.Fatal(status)
	}
	failed := nextPool(t, pool)
	if failed.Status != vfs.StatusBackendError || failed.Count != 0 || !pool.Retire(failed.Ticket) || !pool.Close() {
		t.Fatalf("failure = %+v", failed)
	}
	service.persistent.transport = transport.inner
	ordinary := service.Dispatch(1, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 4})
	if ordinary.Status != vfs.StatusOK || string(ordinary.Data) != "unch" {
		t.Fatalf("cursor after failure = %+v", ordinary)
	}
}

func TestReadPoolRejectsInvalidAdmission(t *testing.T) {
	_, pool, first, _ := poolFixture(t, 1)
	for _, session := range []ReadSession{{}, {Owner: 1}, {Generation: 1}} {
		if _, status := pool.AdmitRead(session, 1, first, 1); status != vfs.StatusInvalidArgument {
			t.Fatalf("invalid session %+v = %d", session, status)
		}
	}
	if _, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 1}, 0, first, 1); status != vfs.StatusInvalidArgument {
		t.Fatalf("zero correlation = %d", status)
	}
	if _, ok := NewReadPool(nil, 1); ok {
		t.Fatal("nil service")
	}
	if _, ok := NewReadPool(&Service{}, 0); ok {
		t.Fatal("zero workers")
	}
	if _, ok := NewReadPool(&Service{}, MaxReadWorkers+1); ok {
		t.Fatal("too many workers")
	}
}

func TestReadPoolBarrierWaitsForQueuedPrefixAndEveryRetirement(t *testing.T) {
	service, store, first, second := blockedRAMFSFixture(t)
	pool, ok := NewReadPool(service, 1)
	if !ok {
		t.Fatal("pool")
	}
	firstTicket, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 1}, 1, first, 4)
	if status != vfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := pool.AdmitRead(ReadSession{Owner: 2, Generation: 1}, 2, second, 4); status != vfs.StatusOK {
		t.Fatal(status)
	}
	barrierTicket, status := pool.AdmitBarrier(ReadSession{}, 3)
	if status != vfs.StatusOK {
		t.Fatal(status)
	}
	waitReadEntry(t, store.entered)
	select {
	case <-store.entered:
		t.Fatal("more than one worker entered RAMFS")
	default:
	}
	// Release only the running read. Poll must then start its queued
	// predecessor before making the barrier visible.
	store.release <- struct{}{}
	completed := nextPool(t, pool)
	if completed.Ticket != firstTicket {
		t.Fatalf("first = %+v", completed)
	}
	waitReadEntry(t, store.entered)
	if _, ok := pool.Poll(); ok {
		t.Fatal("barrier passed a queued predecessor")
	}
	store.release <- struct{}{}
	completed = nextPool(t, pool)
	if completed.Session.Owner != 2 || completed.Barrier {
		t.Fatalf("queued predecessor = %+v", completed)
	}
	if !pool.Retire(completed.Ticket) {
		t.Fatal("retire second")
	}
	if _, ok := pool.Poll(); ok {
		t.Fatal("barrier passed a completed, unretired first read")
	}
	if !pool.Retire(firstTicket) {
		t.Fatal("retire first")
	}
	completed = nextPool(t, pool)
	if completed.Ticket != barrierTicket || !completed.Barrier || !pool.Retire(barrierTicket) || !pool.Close() {
		t.Fatalf("barrier = %+v", completed)
	}
}

func TestReadPoolInlineClampAndEOF(t *testing.T) {
	_, pool, first, _ := poolFixture(t, 2)
	for index, want := range []string{"abcdefgh", ""} {
		if _, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 1}, uint64(index+1), first, ^uint32(0)); status != vfs.StatusOK {
			t.Fatal(status)
		}
		completed := nextPool(t, pool)
		if completed.Status != vfs.StatusOK || completed.Count != len(want) || string(completed.Data) != want || !pool.Retire(completed.Ticket) {
			t.Fatalf("read = %+v, want %q", completed, want)
		}
	}
}

func TestReadPoolCloseSessionRunsOnlyAtBarrier(t *testing.T) {
	service, pool, first, _ := poolFixture(t, 2)
	if _, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 1}, 1, first, 2); status != vfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := pool.AdmitBarrier(ReadSession{}, 2); status != vfs.StatusOK {
		t.Fatal(status)
	}
	read := nextPool(t, pool)
	if read.Barrier || !pool.Retire(read.Ticket) {
		t.Fatal("read retirement")
	}
	barrier := nextPool(t, pool)
	if !barrier.Barrier || service.CloseSession(1) != 1 || !pool.Retire(barrier.Ticket) {
		t.Fatal("close barrier")
	}
	opened := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Read})
	if opened.Status != vfs.StatusOK || opened.Handle == first {
		t.Fatal("reopen")
	}
	if pool.Retire(read.Ticket) {
		t.Fatal("late completion retired reused session")
	}
	if _, status := pool.AdmitRead(ReadSession{Owner: 1, Generation: 2}, 3, opened.Handle, 2); status != vfs.StatusOK {
		t.Fatal(status)
	}
	read = nextPool(t, pool)
	if read.Session.Generation != 2 || string(read.Data) != "ab" || !pool.Retire(read.Ticket) {
		t.Fatalf("new session = %+v", read)
	}
}

type serialReadBackend struct{ *ramfs.FileSystem }

func TestReadPoolUnknownBackendIsASerialBoundary(t *testing.T) {
	service, store, first, _ := blockedRAMFSFixture(t)
	backend := &serialReadBackend{FileSystem: &service.fs}
	if service.namespace.Mount([]byte("/serial"), backend, service.fs.Root()) != vfs.StatusOK {
		t.Fatal("mount")
	}
	serial := service.Dispatch(3, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/serial/tmp/read"), Rights: vfs.Read})
	last := service.Dispatch(4, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/tmp/read"), Rights: vfs.Read})
	if serial.Status != vfs.StatusOK || last.Status != vfs.StatusOK {
		t.Fatal("open")
	}
	pool, ok := NewReadPool(service, 2)
	if !ok {
		t.Fatal("pool")
	}
	for index, entry := range []struct {
		owner vfs.Owner
		file  vfs.Handle
	}{{1, first}, {3, serial.Handle}, {4, last.Handle}} {
		if _, status := pool.AdmitRead(ReadSession{Owner: entry.owner, Generation: 1}, uint64(index+1), entry.file, 4); status != vfs.StatusOK {
			t.Fatal(status)
		}
	}
	for _, owner := range []vfs.Owner{1, 3, 4} {
		waitReadEntry(t, store.entered)
		select {
		case <-store.entered:
			t.Fatal("read passed a serial backend boundary")
		default:
		}
		store.release <- struct{}{}
		completion := nextPool(t, pool)
		if completion.Session.Owner != owner || completion.Status != vfs.StatusOK || string(completion.Data) != "para" {
			t.Fatalf("serial completion = %+v, want owner %d", completion, owner)
		}
		if _, ok := pool.Poll(); ok {
			t.Fatal("next read passed unretired serial work")
		}
		if !pool.Retire(completion.Ticket) {
			t.Fatal("retire")
		}
	}
	if !pool.Close() {
		t.Fatal("close")
	}
}
