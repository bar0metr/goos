package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	"strings"
	"testing"
)

func writePersistent(t *testing.T, service *Service, path string, payload string) {
	t.Helper()
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(path)}); reply.Status != vfs.StatusOK {
		t.Fatalf("create %s = %d", path, reply.Status)
	}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(path)})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open %s = %d", path, opened.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte(payload)}); reply.Status != vfs.StatusOK || reply.Size != uint64(len(payload)) {
		t.Fatalf("write %s = %+v", path, reply)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}

// A filesystem service that dies takes only its own mounts with it, and only
// until it is replaced: open files name nodes by their place on the volume,
// so a restarted service serves the same handles.
func TestFilesystemServiceDeathAndRestart(t *testing.T) {
	device := testFAT32Device()
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) || !service.AttachCache(testCache) {
		t.Fatal("service init failed")
	}
	first := newLoopback(device)
	if !service.AttachFilesystem(first) {
		t.Fatal("volume refused")
	}
	writePersistent(t, service, "/home/note.txt", "survives a restart")
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/home/note.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open = %d", opened.Status)
	}

	first.fail = true
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64},
		{Operation: vfs.OperationStat, Path: []byte("/home/note.txt")},
		{Operation: vfs.OperationReadDir, Path: []byte("/persist")},
		{Operation: vfs.OperationCreate, Path: []byte("/state/new")},
	} {
		if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusPeerDied {
			t.Fatalf("operation %d on a dead service = %d", request.Operation, reply.Status)
		}
	}
	if service.Sync() {
		t.Fatal("sync succeeded without a filesystem service")
	}
	// Everything the dead service does not serve still works.
	for _, path := range []string{"/", "/tmp", "/bootstrap/bin"} {
		if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)}); reply.Status != vfs.StatusOK {
			t.Fatalf("stat %s with the filesystem service dead = %d", path, reply.Status)
		}
	}
	service.DetachFilesystem()
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/home")}); reply.Status != vfs.StatusPeerDied {
		t.Fatalf("stat on a detached volume = %d", reply.Status)
	}

	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("restarted service refused")
	}
	// Object cookies belong to one filesystem-service lifetime, but the VFS
	// knows where each open file's name is and acquires a new object for it
	// from the restarted service: the handle reads the same file.
	read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != "survives a restart" {
		t.Fatalf("read through a handle opened before the restart = %d, %q", read.Status, read.Data)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if got := readAll(t, service, "/home/note.txt"); got != "survives a restart" {
		t.Fatalf("read after reopening on restarted service = %q", got)
	}
	if !service.Sync() {
		t.Fatal("sync failed after the restart")
	}
}

// A reply that does not decode is an I/O error, not a crash or a hang.
func TestMalformedFilesystemReplyIsAnError(t *testing.T) {
	service := startedService(t, nil)
	remote := newLoopback(testFAT32Device())
	if !service.AttachFilesystem(remote) {
		t.Fatal("volume refused")
	}
	remote.corrupt = true
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist")}); reply.Status != vfs.StatusBackendError {
		t.Fatalf("stat with a corrupt reply = %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/tmp")}); reply.Status != vfs.StatusOK {
		t.Fatalf("stat /tmp = %d", reply.Status)
	}
}

// A service restarted onto a different volume is refused rather than served
// under mounts that name another volume's nodes.
func TestRestartedServiceMustServeTheSameVolume(t *testing.T) {
	service := startedService(t, testFAT32Device())
	refused := testFAT32Device()
	refused.data[510] = 0
	if service.AttachFilesystem(newLoopback(refused)) {
		t.Fatal("a service with a refused volume was attached in place of a working one")
	}
}

// Files larger than one protocol message move in several exchanges.
func TestLargeTransfersCrossSeveralExchanges(t *testing.T) {
	service := startedService(t, testFAT32Device())
	payload := strings.Repeat("0123456789abcdef", 64)
	writePersistent(t, service, "/persist/large.txt", payload)
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/large.txt")})
	var read []byte
	for len(read) < len(payload) {
		reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		if reply.Status != vfs.StatusOK || len(reply.Data) == 0 {
			t.Fatalf("read at %d = %d", len(read), reply.Status)
		}
		read = append(read, reply.Data...)
	}
	if string(read) != payload {
		t.Fatal("large file changed across the protocol")
	}
}

// failingDevice stops reading and writing once broken, as a device behind a
// dead driver does.
type failingDevice struct {
	*testBlockDevice
	broken bool
}

func (device *failingDevice) ReadAt(offset uint64, target []byte) bool {
	return !device.broken && device.testBlockDevice.ReadAt(offset, target)
}

func (device *failingDevice) WriteAt(offset uint64, data []byte) bool {
	return !device.broken && device.testBlockDevice.WriteAt(offset, data)
}

func (device *failingDevice) Flush() bool { return !device.broken }

// A block I/O error inside the filesystem service is a controlled error on the
// request that met it, and the service and the VFS keep serving.
func TestBlockIOErrorIsReportedNotFatal(t *testing.T) {
	device := &failingDevice{testBlockDevice: testFAT32Device()}
	service := startedService(t, nil)
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("volume refused")
	}
	writePersistent(t, service, "/persist/io.txt", "before the failure")
	device.broken = true
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/after.txt")}); reply.Status == vfs.StatusOK {
		t.Fatal("a create succeeded on a failed device")
	}
	if service.Sync() {
		t.Fatal("sync succeeded on a failed device")
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/tmp")}); reply.Status != vfs.StatusOK {
		t.Fatalf("stat /tmp during a device failure = %d", reply.Status)
	}
	device.broken = false
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/io.txt")})
	read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != "before the failure" {
		t.Fatalf("read after the device recovered = %d, %q", read.Status, read.Data)
	}
}

// failing reports the same transport failure for every exchange.
type failing struct{ reason ramfs.Status }

func (transport failing) Exchange(fsproto.Request) (fsproto.Reply, ramfs.Status) {
	return fsproto.Reply{}, transport.reason
}

// TestTransportFailuresKeepTheirReason covers the native error model: a
// stalled service is a timeout, a dead one is a dead peer, and a garbled reply
// is an I/O error, each its own status rather than one "storage error".
func TestTransportFailuresKeepTheirReason(t *testing.T) {
	for _, reason := range []ramfs.Status{ramfs.StatusTimeout, ramfs.StatusPeerDied, ramfs.StatusIOError} {
		remote := Remote{}
		remote.transport = failing{reason: reason}
		if _, status := remote.Stat(1); status != reason {
			t.Fatalf("backend reported %d for %d", status, reason)
		}
	}
}

// A file renamed while open is found again at its new place when the
// filesystem service restarts.
func TestRenamedOpenFileSurvivesRestart(t *testing.T) {
	device := testFAT32Device()
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) || !service.AttachCache(testCache) {
		t.Fatal("service init failed")
	}
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("volume refused")
	}
	writePersistent(t, service, "/home/before.txt", "moved then restarted")
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/home/before.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open = %d", opened.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/home/before.txt"), Target: []byte("/home/after.txt")}); reply.Status != vfs.StatusOK {
		t.Fatalf("rename = %d", reply.Status)
	}
	service.DetachFilesystem()
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("restarted service refused")
	}
	read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != "moved then restarted" {
		t.Fatalf("read after rename and restart = %d, %q", read.Status, read.Data)
	}
}
