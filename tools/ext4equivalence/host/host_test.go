package host

import (
	"bytes"
	"testing"

	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
)

func poll(t *testing.T, interests ...Interest) (Result, kcall.Status) {
	t.Helper()
	var objects [MaxObjects]Interest
	copy(objects[:], interests)
	return Any(&objects, len(interests), 0)
}

func TestIPCReceiveValidatesEndpointWithoutConsumingVFS(t *testing.T) {
	Reset()
	Submit([]byte("request"))
	var target [ipc.WireMaxSize]byte
	for _, id := range []handle.ID{0, Reader, Writer, 999} {
		if status := IPCReceive(id, target[:]); status != kcall.StatusPermissionDenied {
			t.Fatalf("receive handle %d: %v", id, status)
		}
	}
	if status := IPCReceive(Block, target[:]); status != kcall.StatusNotFound {
		t.Fatalf("block receive consumed VFS request: %v", status)
	}
	if status := IPCReceive(VFS, target[:1]); status != kcall.StatusInvalidArgument {
		t.Fatalf("short receive buffer: %v", status)
	}
	if status := IPCReceive(VFS, target[:]); status != kcall.StatusOK || !bytes.Equal(target[:7], []byte("request")) {
		t.Fatalf("VFS receive: %v %q", status, target[:7])
	}
	if status := IPCReceive(VFS, target[:]); status != kcall.StatusNotFound {
		t.Fatalf("empty VFS receive: %v", status)
	}
}

func TestIPCSendValidatesEndpointAndQueueBound(t *testing.T) {
	Reset()
	for _, id := range []handle.ID{0, Reader, Writer, 999} {
		if status := IPCSend(id, []byte("wrong endpoint")); status != kcall.StatusPermissionDenied {
			t.Fatalf("send handle %d: %v", id, status)
		}
	}
	if status := IPCSend(Block, []byte("unmodeled block peer")); status != kcall.StatusUnsupported {
		t.Fatalf("block send: %v", status)
	}
	if len(Replies) != 0 {
		t.Fatal("wrong endpoint reached VFS reply queue")
	}
	for i := 0; i < cap(Replies); i++ {
		if status := IPCSend(VFS, []byte("reply")); status != kcall.StatusOK {
			t.Fatalf("send %d: %v", i, status)
		}
	}
	if status := IPCSend(VFS, []byte("overflow")); status != kcall.StatusResourceExhausted {
		t.Fatalf("full reply queue: %v", status)
	}
}

func TestAnyFiltersRequestedEventsAndPreservesMaskPositions(t *testing.T) {
	Reset()
	Submit([]byte("request"))
	if result, status := poll(t, Interest{Handle: VFS, Events: kcall.WaitPeerClosed}); status != kcall.StatusTimeout || result.ReadyMask != 0 {
		t.Fatalf("readable VFS reported peer closure: %+v %v", result, status)
	}
	result, status := poll(t,
		Interest{Handle: Block, Events: kcall.WaitReadable},
		Interest{Handle: VFS, Events: kcall.WaitReadable | kcall.WaitPeerClosed},
	)
	if status != kcall.StatusOK || result.ReadyMask != 2 {
		t.Fatalf("readiness mask: %+v %v", result, status)
	}
}

func TestAnyRejectsInvalidHandlesEventsAndCounts(t *testing.T) {
	Reset()
	for _, interest := range []Interest{
		{Handle: 999, Events: kcall.WaitReadable},
		{Handle: VFS},
		{Handle: VFS, Events: kcall.WaitCompleted},
		{Handle: Reader, Events: kcall.WaitWritable},
		{Handle: Writer, Events: kcall.WaitReadable},
	} {
		if _, status := poll(t, interest); status != kcall.StatusPermissionDenied {
			t.Fatalf("interest %+v: %v", interest, status)
		}
	}
	var interests [MaxObjects]Interest
	for _, count := range []int{-1, MaxObjects + 1} {
		if _, status := Any(&interests, count, 0); status != kcall.StatusInvalidArgument {
			t.Fatalf("count %d: %v", count, status)
		}
	}
	if _, status := Any(nil, 1, 0); status != kcall.StatusInvalidArgument {
		t.Fatalf("nil interests: %v", status)
	}
}

func TestStopReportsOnlyRequestedBlockPeerClosure(t *testing.T) {
	Reset()
	Stop()
	if result, status := poll(t, Interest{Handle: Block, Events: kcall.WaitReadable}); status != kcall.StatusTimeout || result.ReadyMask != 0 {
		t.Fatalf("block closure reported readable: %+v %v", result, status)
	}
	result, status := poll(t,
		Interest{Handle: VFS, Events: kcall.WaitPeerClosed},
		Interest{Handle: Block, Events: kcall.WaitPeerClosed},
	)
	if status != kcall.StatusOK || result.ReadyMask != 2 {
		t.Fatalf("shutdown readiness: %+v %v", result, status)
	}
	var target [ipc.WireMaxSize]byte
	if status := IPCReceive(Block, target[:]); status != kcall.StatusPeerDied {
		t.Fatalf("closed block receive: %v", status)
	}
}

func TestStreamsValidateDirectionAndFilterReadiness(t *testing.T) {
	Reset()
	reader, writer, status := StreamCreate()
	if status != kcall.StatusOK || reader != Reader || writer != Writer {
		t.Fatalf("stream endpoints: %d %d %v", reader, writer, status)
	}
	for _, id := range []handle.ID{0, VFS, Block, Reader, 999} {
		if _, status := StreamWrite(id, []byte{1}); status != kcall.StatusPermissionDenied {
			t.Fatalf("stream write handle %d: %v", id, status)
		}
	}
	for _, id := range []handle.ID{0, VFS, Block, Writer, 999} {
		if _, status := StreamRead(id, make([]byte, 1)); status != kcall.StatusPermissionDenied {
			t.Fatalf("stream read handle %d: %v", id, status)
		}
	}
	if n, status := StreamWrite(Writer, []byte{1, 1}); status != kcall.StatusOK || n != 2 {
		t.Fatalf("wake write: %d %v", n, status)
	}
	if result, status := poll(t, Interest{Handle: Reader, Events: kcall.WaitPeerClosed}); status != kcall.StatusTimeout || result.ReadyMask != 0 {
		t.Fatalf("wake reported closure: %+v %v", result, status)
	}
	if result, status := poll(t, Interest{Handle: Reader, Events: kcall.WaitReadable}); status != kcall.StatusOK || result.ReadyMask != 1 {
		t.Fatalf("wake readiness: %+v %v", result, status)
	}
	if n, status := StreamRead(Reader, make([]byte, 2)); status != kcall.StatusOK || n != 2 {
		t.Fatalf("wake read: %d %v", n, status)
	}
	if _, status := poll(t, Interest{Handle: Reader, Events: kcall.WaitReadable}); status != kcall.StatusTimeout {
		t.Fatalf("drained wake: %v", status)
	}
}
