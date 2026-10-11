package stream

import (
	"testing"

	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/process"
)

const (
	readerRights = capability.Read | capability.Wait | capability.Transfer
	writerRights = capability.Write | capability.Wait | capability.Transfer
)

// created mirrors StreamCreate: one registry slot and one handle per end in
// the creator's table.
func created(t *testing.T, registry *Registry, table *handle.Table) (uint64, handle.ID, handle.ID) {
	t.Helper()
	_, id, ok := registry.Create(1)
	if !ok {
		t.Fatal("stream was not created")
	}
	readerObject, _ := ReaderObject(id)
	writerObject, _ := WriterObject(id)
	reader, readerOK := table.Insert(handle.TypeStream, capability.New(readerObject, readerRights))
	writer, writerOK := table.Insert(handle.TypeStream, capability.New(writerObject, writerRights))
	if !readerOK || !writerOK {
		t.Fatal("endpoint handles were not installed")
	}
	return id, reader, writer
}

// copyHandle mirrors a launch grant that is not consumed: a derived handle in
// another table, counted.
func copyHandle(t *testing.T, registry *Registry, source, target *handle.Table, id handle.ID, rights capability.Rights) handle.ID {
	t.Helper()
	_, cap, found := source.Describe(id)
	if !found {
		t.Fatal("source handle missing")
	}
	copied, ok := source.Transfer(target, id, handle.TypeStream, rights)
	if !ok || !registry.Placed(handle.TypeStream, cap.Object()) {
		t.Fatal("copy was not installed and counted")
	}
	return copied
}

func references(t *testing.T, registry *Registry, id uint64) (uint16, uint16) {
	t.Helper()
	stream, found := registry.Lookup(id)
	if !found {
		t.Fatal("stream is not live")
	}
	return stream.References()
}

func TestSharedWriterStaysOpenUntilEveryHolderCloses(t *testing.T) {
	var registry Registry
	var pid1, stageA, stageB handle.Table
	id, reader, writer := created(t, &registry, &pid1)
	first := copyHandle(t, &registry, &pid1, &stageA, writer, capability.Write|capability.Wait)
	second := copyHandle(t, &registry, &pid1, &stageB, writer, capability.Write|capability.Wait)
	if _, writers := references(t, &registry, id); writers != 3 {
		t.Fatalf("writers = %d, want 3", writers)
	}
	// PID 1 lets go of its own copy; the stages still write.
	if closure, ok := registry.CloseHandle(&pid1, writer); !ok || closure.Closed {
		t.Fatalf("closing one of three writers = %+v, %v", closure, ok)
	}
	stream, _ := registry.Lookup(id)
	if _, status := stream.Write([]byte("from B\n")); status != StatusOK {
		t.Fatalf("write through a shared end = %v", status)
	}
	if closure, ok := registry.CloseHandle(&stageA, first); !ok || closure.Closed {
		t.Fatalf("closing the first stage's writer = %+v, %v", closure, ok)
	}
	if status := stream.ReadStatus(); status != StatusOK {
		t.Fatalf("reader after one holder closed = %v, want data", status)
	}
	buffer := make([]byte, 16)
	if size, status := stream.Read(buffer); status != StatusOK || string(buffer[:size]) != "from B\n" {
		t.Fatalf("read = %q, %v", buffer[:size], status)
	}
	// Drained with a writer still held: no end of input.
	if status := stream.ReadStatus(); status != StatusWouldBlock {
		t.Fatalf("drained stream with a writer held = %v, want would block", status)
	}
	closure, ok := registry.CloseHandle(&stageB, second)
	if !ok || !closure.Closed || !closure.Writer || closure.Stream != id {
		t.Fatalf("closing the last writer = %+v, %v", closure, ok)
	}
	if status := stream.ReadStatus(); status != StatusPeerClosed {
		t.Fatalf("reader after the last writer closed = %v, want end of input", status)
	}
	if _, released := registry.Release(id); released {
		t.Fatal("a stream with an open reader was released")
	}
	if closure, ok := registry.CloseHandle(&pid1, reader); !ok || !closure.Closed || closure.Writer {
		t.Fatalf("closing the reader = %+v, %v", closure, ok)
	}
	if _, released := registry.Release(id); !released {
		t.Fatal("a fully closed stream was not released")
	}
}

// Teardown closes a process's handles through CloseAll; each stream handle it
// visits drops one reference, as releaseProcessHandle does.
func TestTeardownOfOneHolderLeavesTheOthers(t *testing.T) {
	var registry Registry
	var pid1, stage handle.Table
	id, reader, writer := created(t, &registry, &pid1)
	copyHandle(t, &registry, &pid1, &stage, writer, capability.Write|capability.Wait)
	copyHandle(t, &registry, &pid1, &stage, writer, capability.Write|capability.Wait)
	copyHandle(t, &registry, &pid1, &stage, reader, capability.Read|capability.Wait)
	closed := 0
	stage.CloseAll(func(typeID handle.Type, cap capability.Capability) {
		if typeID != handle.TypeStream {
			return
		}
		if _, _, endClosed, ok := registry.Drop(cap.Object()); !ok {
			t.Fatal("teardown drop refused")
		} else if endClosed {
			closed++
		}
	})
	if closed != 0 {
		t.Fatalf("teardown of one holder closed %d ends", closed)
	}
	if readers, writers := references(t, &registry, id); readers != 1 || writers != 1 {
		t.Fatalf("after teardown readers = %d, writers = %d", readers, writers)
	}
	stream, _ := registry.Lookup(id)
	if stream.WriteStatus() != StatusOK || stream.ReadStatus() != StatusWouldBlock {
		t.Fatal("the surviving holder lost its stream")
	}
}

// An IPC transfer moves a handle: the receiver's copy is counted and the
// sender's closed, so the count is unchanged and the end never closes between.
func TestTransferMoveKeepsTheCountAndCopyAddsOne(t *testing.T) {
	var registry Registry
	var sender, receiver handle.Table
	id, _, writer := created(t, &registry, &sender)
	_, writerCap, _ := sender.Describe(writer)
	channel := ipc.NewChannel()
	message := ipc.Message{ProtocolID: 1, Version: 1, Kind: ipc.KindNotification}
	// The kernel strips Transfer from IPC attachments; it is kept here so the
	// receiver can go on to make the copy below, as PID 1 does with its own.
	transfers := []ipc.Transfer{{Handle: writer, Type: handle.TypeStream, Rights: writerRights}}
	if status := channel.Send(0, message, &sender, &receiver, transfers); status != ipc.StatusOK {
		t.Fatalf("send = %v", status)
	}
	if !registry.Placed(handle.TypeStream, writerCap.Object()) {
		t.Fatal("receiver's copy was not counted")
	}
	if closure, ok := registry.CloseHandle(&sender, writer); !ok || closure.Closed {
		t.Fatalf("closing the moved handle = %+v, %v", closure, ok)
	}
	if _, writers := references(t, &registry, id); writers != 1 {
		t.Fatalf("writers after a move = %d, want 1", writers)
	}
	received, status := channel.Receive(1)
	if status != ipc.StatusOK || received.HandleCount != 1 {
		t.Fatal("moved handle not delivered")
	}
	// A copy, as a launch grant that is not consumed makes, adds one.
	var child handle.Table
	copyHandle(t, &registry, &receiver, &child, received.Handles[0], capability.Write)
	if _, writers := references(t, &registry, id); writers != 2 {
		t.Fatalf("writers after a copy = %d, want 2", writers)
	}
}

func TestCountNeverUnderflows(t *testing.T) {
	var registry Registry
	var table handle.Table
	id, reader, writer := created(t, &registry, &table)
	if _, ok := registry.CloseHandle(&table, writer); !ok {
		t.Fatal("first close failed")
	}
	// The handle is gone, so a second close finds nothing and drops nothing.
	if _, ok := registry.CloseHandle(&table, writer); ok {
		t.Fatal("double close succeeded")
	}
	writerObject, _ := WriterObject(id)
	// A drop with nothing counted is refused rather than wrapping to 65535.
	if _, _, _, ok := registry.Drop(writerObject); ok {
		t.Fatal("drop below zero succeeded")
	}
	if _, writers := references(t, &registry, id); writers != 0 {
		t.Fatalf("writers after refused underflow = %d", writers)
	}
	// A closed end cannot be revived by counting a new handle to it.
	if registry.Retain(writerObject) || registry.Placed(handle.TypeStream, writerObject) {
		t.Fatal("a closed end was retained")
	}
	if _, ok := registry.CloseHandle(&table, reader); !ok {
		t.Fatal("reader close failed")
	}
}

func TestForgedEndpointsAreRefused(t *testing.T) {
	var registry Registry
	var table handle.Table
	id, _, _ := created(t, &registry, &table)
	readerObject, _ := ReaderObject(id)
	for name, object := range map[string]uint64{
		"not a stream":          0x4000000000000001,
		"unknown stream":        readerObject + 1,
		"zero":                  0,
		"writer bit on its own": writerEndpointBit,
	} {
		if registry.Retain(object) || registry.CanRetain(object, 1) {
			t.Fatalf("%s: retained", name)
		}
		if _, _, _, ok := registry.Drop(object); ok {
			t.Fatalf("%s: dropped", name)
		}
	}
	// A handle that names no stream is not a stream handle's to close.
	if _, ok := registry.CloseHandle(&table, handle.ID(0x0000000100000077)); ok {
		t.Fatal("a forged handle ID was closed")
	}
	if readers, writers := references(t, &registry, id); readers != 1 || writers != 1 {
		t.Fatalf("forged operations changed the counts: %d, %d", readers, writers)
	}
}

func TestCountIsBounded(t *testing.T) {
	stream := New()
	stream.writers = MaxReferences - 1
	if !stream.CanRetain(true, 1) || stream.CanRetain(true, 2) {
		t.Fatal("headroom misreported")
	}
	if !stream.Retain(true) || stream.Retain(true) {
		t.Fatal("the count passed its bound")
	}
	if _, writers := stream.References(); writers != MaxReferences {
		t.Fatalf("writers = %d", writers)
	}
}

// The counter cannot overflow in practice: every handle lives in one of the
// process tables, so no end can have more than their total.
func TestTablesCannotReachTheBound(t *testing.T) {
	if process.MaxProcesses*handle.MaxHandles >= MaxReferences {
		t.Fatalf("%d handle slots can overflow a %d-reference count", process.MaxProcesses*handle.MaxHandles, MaxReferences)
	}
}

func TestSmallWritesAreAtomic(t *testing.T) {
	stream := New()
	filler := make([]byte, Capacity-100)
	if size, status := stream.Write(filler); status != StatusOK || size != len(filler) {
		t.Fatalf("fill = %d, %v", size, status)
	}
	// 100 bytes are free: a 200-byte write must wait rather than land split.
	if size, status := stream.Write(make([]byte, 200)); status != StatusWouldBlock || size != 0 {
		t.Fatalf("split write = %d, %v", size, status)
	}
	if size, status := stream.Write(make([]byte, 100)); status != StatusOK || size != 100 {
		t.Fatalf("fitting write = %d, %v", size, status)
	}
	// Writable is reported only when a whole atomic write fits, so a woken
	// writer is never refused again.
	if status := stream.WriteStatus(); status != StatusWouldBlock {
		t.Fatalf("full stream readiness = %v", status)
	}
	stream.Read(make([]byte, AtomicWrite))
	if status := stream.WriteStatus(); status != StatusOK {
		t.Fatalf("readiness with room for an atomic write = %v", status)
	}
}

func TestDuplicateStreamEndpointKeepsNarrowRightsAndReferences(t *testing.T) {
	var registry Registry
	var table handle.Table
	id, reader, writer := created(t, &registry, &table)
	duplicate, status := registry.DuplicateHandle(&table, reader, capability.Read|capability.Wait|capability.Transfer)
	if status != kcall.StatusOK {
		t.Fatalf("duplicate status %d", status)
	}
	readers, writers := references(t, &registry, id)
	if readers != 2 || writers != 1 {
		t.Fatal("duplicate not retained")
	}
	if _, status := registry.DuplicateHandle(&table, reader, capability.Write); status != kcall.StatusPermissionDenied {
		t.Fatal("reader gained writer authority")
	}
	if _, found := table.Lookup(duplicate, handle.TypeStream, capability.Write); found {
		t.Fatal("duplicate can write")
	}
	if _, closed := registry.CloseHandle(&table, reader); !closed {
		t.Fatal("creator reader close")
	}
	stream, _ := registry.Lookup(id)
	if !stream.readerOpen {
		t.Fatal("shared reader closed with creator")
	}
	registry.CloseHandle(&table, writer)
	closure, closed := registry.CloseHandle(&table, duplicate)
	if !closed || !closure.Closed {
		t.Fatal("last duplicate not closed")
	}
	if _, released := registry.Release(id); !released {
		t.Fatal("stream allocation leaked")
	}
}

func TestResizeStreamRepeatedReapAndDisconnectReturnHandleBudget(t *testing.T) {
	var registry Registry
	var parent, child handle.Table
	parentBudget, childBudget := parent.Available(), child.Available()
	for iteration := 0; iteration < 128; iteration++ {
		id, reader, writer := created(t, &registry, &parent)
		receiver := copyHandle(t, &registry, &parent, &child, reader, capability.Read|capability.Wait)
		if iteration%2 == 0 {
			registry.CloseHandle(&child, receiver)
			registry.CloseHandle(&parent, reader)
			registry.CloseHandle(&parent, writer)
		} else {
			registry.CloseHandle(&parent, writer)
			registry.CloseHandle(&parent, reader)
			registry.CloseHandle(&child, receiver)
		}
		if _, released := registry.Release(id); !released {
			t.Fatalf("stream leaked iteration%d", iteration)
		}
		if parent.Available() != parentBudget || child.Available() != childBudget {
			t.Fatalf("handle budget leaked iteration%d", iteration)
		}
	}
}

func TestRestoredResizeReceiverTransfersAsReadWaitAndReleases(t *testing.T) {
	var registry Registry
	var supervisor, parent handle.Table
	id, reader, writer := created(t, &registry, &supervisor)
	copy, status := registry.DuplicateHandle(&supervisor, reader, capability.Read|capability.Wait|capability.Transfer)
	if status != kcall.StatusOK {
		t.Fatal("duplicate")
	}
	typeID, cap, found := supervisor.Describe(copy)
	if !found {
		t.Fatal("duplicate absent")
	}
	channel := ipc.NewChannel()
	message := ipc.Message{ProtocolID: 1, Version: 1, Kind: ipc.KindNotification}
	transfers := []ipc.Transfer{{Handle: copy, Type: typeID, Rights: cap.Rights() &^ capability.Transfer}}
	if channel.Send(0, message, &supervisor, &parent, transfers) != ipc.StatusOK {
		t.Fatal("restore transfer")
	}
	if !registry.Placed(typeID, cap.Object()) {
		t.Fatal("recipient reference")
	}
	registry.CloseHandle(&supervisor, copy)
	received, receiveStatus := channel.Receive(1)
	if receiveStatus != ipc.StatusOK {
		t.Fatal("restore notice")
	}
	receiver := received.Handles[0]
	if _, valid := parent.Lookup(receiver, handle.TypeStream, capability.Read|capability.Wait); !valid {
		t.Fatal("receive rights missing")
	}
	for _, forbidden := range []capability.Rights{capability.Write, capability.Transfer} {
		if _, valid := parent.Lookup(receiver, handle.TypeStream, forbidden); valid {
			t.Fatal("restored receiver has extra authority")
		}
	}
	registry.CloseHandle(&supervisor, writer)
	registry.CloseHandle(&supervisor, reader)
	registry.CloseHandle(&parent, receiver)
	if _, released := registry.Release(id); !released {
		t.Fatal("restored receiver leaked allocation")
	}
}
