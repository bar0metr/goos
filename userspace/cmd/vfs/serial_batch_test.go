package main

import (
	"bytes"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	vfsservice "goos/userspace/vfs"
	"testing"
)

type batchVolume struct{ *ramfs.FileSystem }

func (volume batchVolume) Root() (ramfs.NodeID, ramfs.Status) {
	return volume.FileSystem.Root(), ramfs.StatusOK
}
func (batchVolume) Flush() bool { return true }
func (batchVolume) AcquireObject(node ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	return node, ramfs.StatusOK
}
func (batchVolume) ReleaseObject(node ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	return node, ramfs.StatusOK
}

// batchTransport models an accepted request whose reply never arrives. Its
// availability contract matches the native transport: later Exchange calls
// reject offline state, but the owner must first detach Remote so a later
// Write cannot overwrite shared bytes before it calls Exchange.
type batchTransport struct {
	volume       batchVolume
	cache        []byte
	name         [ramfs.MaxName]byte
	availability filesystemAvailability
	timeout      fsproto.Operation
}

func (transport *batchTransport) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if failure := transport.availability.failure(); failure != ramfs.StatusOK {
		return fsproto.Reply{}, failure
	}
	if request.Operation == transport.timeout {
		transport.availability.fail(ramfs.StatusTimeout, true)
		return fsproto.Reply{}, ramfs.StatusTimeout
	}
	return fsproto.Serve(transport.volume, request, transport.cache, &transport.name), ramfs.StatusOK
}

func serialBatchFixture(t *testing.T) (*vfsservice.Service, *batchTransport, [2]vfs.Handle, func()) {
	t.Helper()
	fs := &ramfs.FileSystem{}
	if fs.Init() == 0 {
		t.Fatal("filesystem init")
	}
	transport := &batchTransport{volume: batchVolume{FileSystem: fs}, cache: make([]byte, fsproto.CacheSize)}
	service := &vfsservice.Service{}
	if !service.Init() || !service.AttachCache(transport.cache) || !service.AttachFilesystem(transport) {
		t.Fatal("service init")
	}
	if reply := service.Dispatch(1, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/home/file")}); reply.Status != vfs.StatusOK {
		t.Fatal(reply.Status)
	}
	var files [2]vfs.Handle
	for index := range files {
		reply := service.Dispatch(vfs.Owner(index+1), vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/home/file"), Rights: vfs.Write})
		if reply.Status != vfs.StatusOK {
			t.Fatal(reply.Status)
		}
		files[index] = reply.Handle
	}
	before := func() {
		if transport.availability.failure() != ramfs.StatusOK {
			service.DetachFilesystem()
		}
	}
	return service, transport, files, before
}

func TestSerialBatchTimeoutThenSecondWriterDoesNotReuseCache(t *testing.T) {
	service, transport, files, before := serialBatchFixture(t)
	transport.timeout = fsproto.OperationWrite
	var frozen []byte
	dispatchSerialBatch(1<<1|1<<2, 3, before, nil, func(interest int) {
		data := []byte("first-in-flight")
		want := vfs.StatusTimeout
		if interest == 2 {
			data, want = []byte("second-overwrite"), vfs.StatusPeerDied
		}
		reply := service.Dispatch(vfs.Owner(interest), vfs.Request{Operation: vfs.OperationWrite, Handle: files[interest-1], Data: data})
		if interest == 1 {
			if !transport.availability.quarantined.Load() {
				t.Fatal("accepted timeout did not quarantine cache")
			}
			frozen = append([]byte(nil), transport.cache...)
		} else if !bytes.Equal(transport.cache, frozen) {
			t.Fatal("second ready writer overwrote cache used by an unanswered request")
		}
		if reply.Status != want || reply.Size != 0 {
			t.Fatalf("session %d: reply=%+v, want status %d", interest, reply, want)
		}
	})
}

func TestSerialBatchControlTimeoutDetachesBeforeReadyWriter(t *testing.T) {
	service, transport, files, before := serialBatchFixture(t)
	transport.timeout = fsproto.OperationFlush
	frozen := append([]byte(nil), transport.cache...)
	dispatchSerialBatch(1|1<<1, 2, before, func() {
		if service.Sync() || !transport.availability.quarantined.Load() {
			t.Fatal("control did not model an accepted flush timeout")
		}
	}, func(int) {
		reply := service.Dispatch(1, vfs.Request{Operation: vfs.OperationWrite, Handle: files[0], Data: []byte("unsafe-after-control")})
		if !bytes.Equal(transport.cache, frozen) {
			t.Fatal("ready session reused cache after control timed out in the same batch")
		}
		if reply.Status != vfs.StatusPeerDied || reply.Size != 0 {
			t.Fatalf("write after control timeout = %+v", reply)
		}
	})
}

func TestSerialBatchAppliesOfflineBeforeEveryReadyAction(t *testing.T) {
	var order []int
	dispatchSerialBatch(1|1<<1|1<<3, 4, func() {
		order = append(order, 0)
	}, func() {
		order = append(order, -1)
	}, func(interest int) {
		order = append(order, interest)
	})
	want := []int{0, -1, 0, 1, 0, 3}
	if len(order) != len(want) {
		t.Fatalf("dispatch order = %v, want %v", order, want)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("dispatch order = %v, want %v", order, want)
		}
	}
}

// A quarantined cache stays detached for an unfenced attach and comes back
// for a fenced one, with the files the sessions opened before the timeout.
func TestSerialBatchQuarantineRecoversBehindFence(t *testing.T) {
	service, transport, files, before := serialBatchFixture(t)
	transport.timeout = fsproto.OperationWrite
	dispatchSerialBatch(1<<1, 2, before, nil, func(int) {
		service.Dispatch(1, vfs.Request{Operation: vfs.OperationWrite, Handle: files[0], Data: []byte("lost")})
	})
	before()
	if !transport.availability.quarantined.Load() {
		t.Fatal("accepted timeout did not quarantine cache")
	}
	// The replacement shares the same cache object, as the native one does.
	replacement := &batchTransport{volume: transport.volume, cache: transport.cache}
	attach := func(fenced bool) bool {
		allowed, _ := transport.availability.admit(fenced)
		return allowed && service.AttachFilesystem(replacement)
	}
	if attach(false) {
		t.Fatal("unfenced attach reused a quarantined cache")
	}
	frozen := append([]byte(nil), transport.cache...)
	if reply := service.Dispatch(2, vfs.Request{Operation: vfs.OperationWrite, Handle: files[1], Data: []byte("refused")}); reply.Status != vfs.StatusPeerDied || !bytes.Equal(transport.cache, frozen) {
		t.Fatalf("write after a refused attach = %+v", reply)
	}
	if !attach(true) {
		t.Fatal("fenced attach refused")
	}
	if reply := service.Dispatch(2, vfs.Request{Operation: vfs.OperationWrite, Handle: files[1], Data: []byte("recovered")}); reply.Status != vfs.StatusOK || reply.Size != 9 {
		t.Fatalf("write through an open file after recovery = %+v", reply)
	}
	opened := service.Dispatch(3, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/home/file"), Rights: vfs.Read})
	if opened.Status != vfs.StatusOK {
		t.Fatal(opened.Status)
	}
	reply := service.Dispatch(3, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if reply.Status != vfs.StatusOK || string(reply.Data) != "recovered" {
		t.Fatalf("read after recovery = %+v", reply)
	}
}
