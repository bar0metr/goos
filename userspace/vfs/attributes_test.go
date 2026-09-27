package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	"testing"
)

// counting records how many exchanges reach the service.
type counting struct {
	inner     *loopback
	exchanges int
}

func (transport *counting) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	transport.exchanges++
	return transport.inner.Exchange(request)
}

func dispatchCounted(t *testing.T, service *Service, transport *counting, request vfs.Request) (vfs.Reply, int) {
	t.Helper()
	before := transport.exchanges
	reply := service.Dispatch(testSession, request)
	return reply, transport.exchanges - before
}

// Attributes the service already sent are not asked for again, and the end of
// a file is known without an exchange.
func TestAttributeCacheSavesExchanges(t *testing.T) {
	service := startedService(t, nil)
	transport := &counting{inner: newLoopback(testFAT32Device())}
	if !service.AttachFilesystem(transport) {
		t.Fatal("volume refused")
	}
	path := []byte("/persist/cached.txt")
	if reply, _ := dispatchCounted(t, service, transport, vfs.Request{Operation: vfs.OperationCreate, Path: path}); reply.Status != vfs.StatusOK {
		t.Fatalf("create = %d", reply.Status)
	}
	opened, exchanges := dispatchCounted(t, service, transport, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.Write, Path: path})
	// Opening a persistent file looks up its attributes and acquires its
	// stable filesystem object.
	if opened.Status != vfs.StatusOK || exchanges > 2 {
		t.Fatalf("open = %d with %d exchanges", opened.Status, exchanges)
	}
	if reply, _ := dispatchCounted(t, service, transport, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte("twelve bytes")}); reply.Status != vfs.StatusOK {
		t.Fatalf("write = %d", reply.Status)
	}
	stat, exchanges := dispatchCounted(t, service, transport, vfs.Request{Operation: vfs.OperationStat, Path: path})
	if stat.Status != vfs.StatusOK || stat.Size != 12 || exchanges != 1 {
		t.Fatalf("stat after write = %d, size %d, %d exchanges", stat.Status, stat.Size, exchanges)
	}
	// The handle's offset is at the end after the write: the read is known to
	// be empty.
	read, exchanges := dispatchCounted(t, service, transport, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 16})
	if read.Status != vfs.StatusOK || len(read.Data) != 0 || exchanges != 0 {
		t.Fatalf("read at the end = %d, %d bytes, %d exchanges", read.Status, len(read.Data), exchanges)
	}
}

// The cache never answers with what the volume no longer holds.
func TestAttributeCacheFollowsChanges(t *testing.T) {
	device := testFAT32Device()
	service := startedService(t, device)
	writePersistent(t, service, "/persist/a.txt", "0123456789")
	stat := func(path string) vfs.Reply {
		t.Helper()
		return service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)})
	}
	if reply := stat("/persist/a.txt"); reply.Status != vfs.StatusOK || reply.Size != 10 {
		t.Fatalf("stat = %d, %d", reply.Status, reply.Size)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/persist/a.txt"), Offset: 4}); reply.Status != vfs.StatusOK {
		t.Fatalf("truncate = %d", reply.Status)
	}
	if reply := stat("/persist/a.txt"); reply.Size != 4 {
		t.Fatalf("size after truncate = %d", reply.Size)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/persist/a.txt"), Target: []byte("/persist/b.txt")}); reply.Status != vfs.StatusOK {
		t.Fatalf("rename = %d", reply.Status)
	}
	if reply := stat("/persist/a.txt"); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stat of the old name = %d", reply.Status)
	}
	if reply := stat("/persist/b.txt"); reply.Status != vfs.StatusOK || reply.Size != 4 {
		t.Fatalf("stat of the new name = %d, %d", reply.Status, reply.Size)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/b.txt")}); reply.Status != vfs.StatusOK {
		t.Fatalf("unlink = %d", reply.Status)
	}
	if reply := stat("/persist/b.txt"); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stat after unlink = %d", reply.Status)
	}
	// What the cache says agrees with a fresh look at the volume.
	writePersistent(t, service, "/persist/c.txt", "abc")
	fresh := startedService(t, device)
	if cached, direct := stat("/persist/c.txt"), fresh.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/c.txt")}); cached.Size != direct.Size || cached.Kind != direct.Kind {
		t.Fatalf("cached %+v, volume %+v", cached, direct)
	}
}
