package vfsservice

import (
	"archive/tar"
	"bytes"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fat32"
	"testing"
	"time"
)

// testSystemImage builds the shape the real boot archive has: a system
// directory, its message of the day, and one program reachable under several
// names in system/bin.
func testSystemImage(t testing.TB) []byte {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := tar.NewWriter(buffer)
	epoch := time.Unix(0, 0)
	program := []byte("PROGRAM")
	motd := []byte("Welcome to GoOS\n")
	headers := []tar.Header{
		{Name: "system/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/bin/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/motd", Mode: 0o444, Size: int64(len(motd)), Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/bin/coreutils", Mode: 0o555, Size: int64(len(program)), Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/bin/ls", Mode: 0o555, Typeflag: tar.TypeLink, Linkname: "system/bin/coreutils", ModTime: epoch, Format: tar.FormatUSTAR},
	}
	for index := range headers {
		if err := writer.WriteHeader(&headers[index]); err != nil {
			t.Fatalf("header %s: %v", headers[index].Name, err)
		}
		if headers[index].Name == "system/motd" {
			if _, err := writer.Write(motd); err != nil {
				t.Fatalf("motd: %v", err)
			}
		} else if headers[index].Name == "system/bin/coreutils" {
			if _, err := writer.Write(program); err != nil {
				t.Fatalf("program: %v", err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buffer.Bytes()
}

// startedService is a service with the mounts a booted system has. Its
// persistent volume is served by a FAT32 service over the protocol the real
// one speaks, run in memory (see loopback).
func startedService(t testing.TB, device fat32.Device) *Service {
	t.Helper()
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) || !service.AttachCache(testCache) {
		t.Fatal("service init failed")
	}
	if device != nil && !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("persistent mount failed")
	}
	return service
}

type testBlockDevice struct{ data []byte }

func (device *testBlockDevice) ReadAt(offset uint64, target []byte) bool {
	return offset+uint64(len(target)) <= uint64(len(device.data)) && copy(target, device.data[offset:]) == len(target)
}
func (device *testBlockDevice) WriteAt(offset uint64, data []byte) bool {
	return offset+uint64(len(data)) <= uint64(len(device.data)) && copy(device.data[offset:], data) == len(data)
}
func (*testBlockDevice) Flush() bool             { return true }
func (device *testBlockDevice) Capacity() uint64 { return uint64(len(device.data)) }

func testFAT32Device() *testBlockDevice {
	device := &testBlockDevice{data: make([]byte, 4*1024*1024)}
	boot := device.data[:512]
	boot[11], boot[12], boot[13] = 0, 2, 1
	boot[14], boot[15], boot[16] = 32, 0, 1
	boot[21] = 0xf8
	boot[32], boot[33] = 0, 32
	boot[36], boot[44] = 64, 2
	boot[510], boot[511] = 0x55, 0xaa
	fat := device.data[32*512:]
	fat[0], fat[1], fat[2], fat[3] = 0xf8, 0xff, 0xff, 0x0f
	fat[4], fat[5], fat[6], fat[7] = 0xff, 0xff, 0xff, 0xff
	fat[8], fat[9], fat[10], fat[11] = 0xff, 0xff, 0xff, 0x0f
	return device
}

func TestServiceFilesystemLifecycle(t *testing.T) {
	service := startedService(t, nil)
	stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/bootstrap")})
	if stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		t.Fatalf("stat = %+v", stat)
	}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/bootstrap/motd")})
	if opened.Status != vfs.StatusOK || opened.Handle == 0 {
		t.Fatalf("open = %+v", opened)
	}
	read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != "Welcome to GoOS\n" {
		t.Fatalf("read = %d, %q", read.Status, read.Data)
	}
	if closed := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle}); closed.Status != vfs.StatusOK {
		t.Fatalf("close = %d", closed.Status)
	}
}

func TestServiceReadDirIsBounded(t *testing.T) {
	service := startedService(t, nil)
	first := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/"), Offset: 0})
	if first.Status != vfs.StatusOK || first.Cursor == 0 || len(first.Name) == 0 {
		t.Fatalf("readdir = %+v", first)
	}
}

func TestServicePersistentFileLifecycle(t *testing.T) {
	service := startedService(t, testFAT32Device())
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/state.txt")}); reply.Status != vfs.StatusOK {
		t.Fatalf("create = %d", reply.Status)
	}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte("/persist/state.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open write = %d", opened.Status)
	}
	payload := []byte("Welcome to GoOS\n")
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: payload}); reply.Status != vfs.StatusOK || reply.Size != uint64(len(payload)) {
		t.Fatalf("write = %+v", reply)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	opened = service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/state.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open read = %d", opened.Status)
	}
	read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != string(payload) {
		t.Fatalf("read = %d, %q", read.Status, read.Data)
	}
}

func TestPersistentOpenObjectSurvivesMoveAndBlocksUnlink(t *testing.T) {
	service := startedService(t, testFAT32Device())
	for _, path := range []string{"/persist/a", "/persist/b"} {
		if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte(path)}); reply.Status != vfs.StatusOK {
			t.Fatalf("mkdir %s = %d", path, reply.Status)
		}
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/a/file")}); reply.Status != vfs.StatusOK {
		t.Fatalf("create = %d", reply.Status)
	}
	writeAt(t, service, "/persist/a/file", 0, "payload")
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/a/file")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open = %d", opened.Status)
	}
	second := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte("/persist/a/file")})
	if second.Status != vfs.StatusOK {
		t.Fatalf("second open = %d", second.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: second.Handle, Data: []byte("P")}); reply.Status != vfs.StatusOK {
		t.Fatalf("write through second handle = %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: second.Handle}); reply.Status != vfs.StatusOK {
		t.Fatalf("close second handle = %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/persist/a/file"), Target: []byte("/persist/b/moved")}); reply.Status != vfs.StatusOK {
		t.Fatalf("rename = %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 16}); reply.Status != vfs.StatusOK || string(reply.Data) != "Payload" {
		t.Fatalf("read moved open file = %d, %q", reply.Status, reply.Data)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/b/moved")}); reply.Status != vfs.StatusPermissionDenied {
		t.Fatalf("unlink open file = %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle}); reply.Status != vfs.StatusOK {
		t.Fatalf("close = %d", reply.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/b/moved")}); reply.Status != vfs.StatusOK {
		t.Fatalf("unlink closed file = %d", reply.Status)
	}
}

func TestServicePersistentSubdirectoryLifecycle(t *testing.T) {
	device := testFAT32Device()
	service := startedService(t, device)
	dispatch := func(request vfs.Request) vfs.Reply {
		t.Helper()
		reply := service.Dispatch(testSession, request)
		if reply.Status != vfs.StatusOK {
			t.Fatalf("%d %s = %d", request.Operation, request.Path, reply.Status)
		}
		return reply
	}
	path := []byte("/persist/demo/message.txt")
	dispatch(vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/persist/demo")})
	dispatch(vfs.Request{Operation: vfs.OperationCreate, Path: path})
	dispatch(vfs.Request{Operation: vfs.OperationTruncate, Path: path, Offset: 0})
	opened := dispatch(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	dispatch(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte("persistent\n")})
	dispatch(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if stat := dispatch(vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/demo")}); stat.Kind != ramfs.KindDirectory {
		t.Fatalf("stat kind = %d", stat.Kind)
	}
	listed := dispatch(vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/persist/demo")})
	if string(listed.Name) != "message.txt" || listed.Size != 11 {
		t.Fatalf("readdir = %q %d", listed.Name, listed.Size)
	}
	if end := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/persist/demo"), Offset: uint64(listed.Cursor)}); end.Status != vfs.StatusNotFound {
		t.Fatalf("readdir end = %d", end.Status)
	}

	service = startedService(t, device)
	opened = dispatch(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if read := dispatch(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64}); string(read.Data) != "persistent\n" {
		t.Fatalf("read = %q", read.Data)
	}
	dispatch(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/demo")}); reply.Status == vfs.StatusOK {
		t.Fatal("removed non-empty directory")
	}
	dispatch(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
	dispatch(vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/demo")})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/demo")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stat removed = %d", reply.Status)
	}
}

func TestUserDataAndStateHaveTheirOwnMounts(t *testing.T) {
	device := testFAT32Device()
	service := startedService(t, device)
	for _, path := range []string{"/home", "/state"} {
		stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)})
		if stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindDirectory {
			t.Fatalf("stat %s = %d %d", path, stat.Status, stat.Kind)
		}
	}

	// The root listing names every mount, so the layout is discoverable.
	names := map[string]bool{}
	for cursor := uint32(0); ; {
		reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/"), Offset: uint64(cursor)})
		if reply.Status == vfs.StatusNotFound {
			break
		}
		if reply.Status != vfs.StatusOK || reply.Cursor <= cursor {
			t.Fatalf("readdir / = %d cursor %d", reply.Status, reply.Cursor)
		}
		names[string(reply.Name)] = true
		cursor = reply.Cursor
	}
	for _, wanted := range []string{"bootstrap", "tmp", "run", "persist", "home", "state"} {
		if !names[wanted] {
			t.Fatalf("root listing %v is missing %q", names, wanted)
		}
	}

	// Writing through /home lands on the volume and survives a remount.
	write := func(path string, payload string) {
		t.Helper()
		if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(path)}); reply.Status != vfs.StatusOK {
			t.Fatalf("create %s = %d", path, reply.Status)
		}
		opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(path)})
		if opened.Status != vfs.StatusOK {
			t.Fatalf("open %s = %d", path, opened.Status)
		}
		if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte(payload)}); reply.Status != vfs.StatusOK {
			t.Fatalf("write %s = %d", path, reply.Status)
		}
		service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	}
	read := func(path string) string {
		t.Helper()
		opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(path)})
		if opened.Status != vfs.StatusOK {
			t.Fatalf("open %s = %d", path, opened.Status)
		}
		reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
		if reply.Status != vfs.StatusOK {
			t.Fatalf("read %s = %d", path, reply.Status)
		}
		service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
		return string(reply.Data)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/demo")}); reply.Status != vfs.StatusOK {
		t.Fatalf("mkdir /home/demo = %d", reply.Status)
	}
	write("/home/demo/message.txt", "persistent")

	// /persist is the same volume, so the file is reachable under both names.
	if value := read("/persist/home/demo/message.txt"); value != "persistent" {
		t.Fatalf("aliased read = %q", value)
	}

	service = startedService(t, device)
	if value := read("/home/demo/message.txt"); value != "persistent" {
		t.Fatalf("read after remount = %q", value)
	}
}

func TestPersistentMountsSurviveDirectoryRemoval(t *testing.T) {
	service := startedService(t, testFAT32Device())
	for _, path := range []string{"/home", "/persist/home", "/state", "/persist"} {
		if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte(path)}); reply.Status == vfs.StatusOK {
			t.Fatalf("removing %s orphaned a mount", path)
		}
	}
	if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/home")}); stat.Status != vfs.StatusOK {
		t.Fatalf("/home lost: %d", stat.Status)
	}
}

func TestSystemImageIsVisibleAndImmutable(t *testing.T) {
	service := startedService(t, testFAT32Device())

	// The executables the launch path resolves are the ones a listing shows.
	names := map[string]bool{}
	for cursor := uint32(0); ; {
		reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/bootstrap/bin"), Offset: uint64(cursor)})
		if reply.Status == vfs.StatusNotFound {
			break
		}
		if reply.Status != vfs.StatusOK || reply.Cursor <= cursor {
			t.Fatalf("readdir /bootstrap/bin = %d cursor %d", reply.Status, reply.Cursor)
		}
		names[string(reply.Name)] = true
		cursor = reply.Cursor
	}
	if !names["ls"] || !names["coreutils"] {
		t.Fatalf("/bootstrap/bin listing = %v", names)
	}
	if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/bootstrap/bin/ls")}); stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindFile || stat.Size != 7 {
		t.Fatalf("stat /bootstrap/bin/ls = %+v", stat)
	}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/bootstrap/motd")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open motd = %d", opened.Status)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64}); string(reply.Data) != "Welcome to GoOS\n" {
		t.Fatalf("read motd = %q", reply.Data)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})

	// Nothing can change the system image through the namespace.
	for _, request := range []vfs.Request{
		{Operation: vfs.OperationCreate, Path: []byte("/bootstrap/bin/backdoor")},
		{Operation: vfs.OperationMkdir, Path: []byte("/bootstrap/extra")},
		{Operation: vfs.OperationUnlink, Path: []byte("/bootstrap/motd")},
		{Operation: vfs.OperationTruncate, Path: []byte("/bootstrap/motd"), Offset: 0},
		{Operation: vfs.OperationRename, Path: []byte("/bootstrap/motd"), Target: []byte("/bootstrap/banner")},
	} {
		if reply := service.Dispatch(testSession, request); reply.Status != vfs.StatusPermissionDenied {
			t.Fatalf("mutation %d of %s = %d", request.Operation, request.Path, reply.Status)
		}
	}
	writable := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte("/bootstrap/motd")})
	if writable.Status == vfs.StatusOK {
		if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: writable.Handle, Data: []byte("x")}); reply.Status != vfs.StatusPermissionDenied {
			t.Fatalf("write to the system image = %d", reply.Status)
		}
		service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: writable.Handle})
	}
}

func TestMalformedSystemImageIsRefused(t *testing.T) {
	image := testSystemImage(t)
	image[0] = 'x'
	if (&Service{}).MountBootstrap(image) {
		t.Fatal("corrupt system image mounted")
	}
	if (&Service{}).MountBootstrap(nil) {
		t.Fatal("empty system image mounted")
	}
}

func TestRejectedVolumeLeavesTheRestOfTheNamespace(t *testing.T) {
	device := testFAT32Device()
	// Claim a volume far larger than the device holds.
	device.data[32], device.data[33], device.data[34], device.data[35] = 0, 0, 0, 0x20
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) {
		t.Fatal("service init failed")
	}
	if service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("an oversized volume was mounted")
	}
	// A refused volume must not take the rest of the namespace with it: the
	// system has to come up to a prompt from which the volume can be looked at.
	for _, path := range []string{"/", "/tmp", "/bootstrap", "/bootstrap/bin"} {
		if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)}); stat.Status != vfs.StatusOK {
			t.Fatalf("stat %s after a refused mount = %d", path, stat.Status)
		}
	}
	for _, path := range []string{"/persist", "/home", "/state"} {
		if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)}); stat.Status != vfs.StatusNotFound {
			t.Fatalf("stat %s after a refused mount = %d", path, stat.Status)
		}
	}
	// The volatile root still works, so the system is usable without storage.
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/tmp/work")}); reply.Status != vfs.StatusOK {
		t.Fatalf("mkdir on the volatile root = %d", reply.Status)
	}
}

func TestPartiallyMountedVolumeIsNotHalfInstalled(t *testing.T) {
	device := testFAT32Device()
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) {
		t.Fatal("service init failed")
	}
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("a well-formed volume was refused")
	}
	// Attaching again, as after a service restart, must not install a second
	// set of the same mounts.
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("a restarted service was refused")
	}
	for _, path := range []string{"/persist", "/home", "/state"} {
		if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)}); stat.Status != vfs.StatusOK {
			t.Fatalf("stat %s = %d", path, stat.Status)
		}
	}
}

func TestCleanUnmountFlushesAndRejectsLiveReferences(t *testing.T) {
	service := startedService(t, testFAT32Device())
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/live")})
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/live")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open = %d", opened.Status)
	}
	if status := service.UnmountFilesystem(); status != vfs.StatusBusy {
		t.Fatalf("unmount with live file = %d", status)
	}
	if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/home")}); stat.Status != vfs.StatusOK {
		t.Fatalf("busy unmount changed namespace: %d", stat.Status)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if status := service.UnmountFilesystem(); status != vfs.StatusOK || service.PersistentMounted() {
		t.Fatalf("clean unmount = %d mounted=%v", status, service.PersistentMounted())
	}
	for _, path := range []string{"/persist", "/home", "/state"} {
		if stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte(path)}); stat.Status != vfs.StatusNotFound {
			t.Fatalf("%s remains after unmount: %d", path, stat.Status)
		}
	}
}

// testSession is the session the tests' requests arrive on.
const testSession = vfs.Owner(1)

func TestSessionFilesAreItsOwn(t *testing.T) {
	service := startedService(t, nil)
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/shared")})
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/tmp/shared")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open = %d", opened.Status)
	}
	if reply := service.Dispatch(testSession+1, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 8}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("another session read the file: %d", reply.Status)
	}
	if closed := service.CloseSession(testSession); closed != 1 {
		t.Fatalf("closed %d files", closed)
	}
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 8}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("file of a gone session still open: %d", reply.Status)
	}
}

// A stat with a handle and no path follows the open file: it reports the
// size as writes change it, survives a rename, belongs to its session, and
// ends with the handle.
func TestServiceStatByHandleFollowsTheOpenFile(t *testing.T) {
	service := startedService(t, nil)
	path := []byte("/tmp/by-handle")
	if created := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: path}); created.Status != vfs.StatusOK {
		t.Fatalf("create = %d", created.Status)
	}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.Write, Path: path})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open = %d", opened.Status)
	}
	byHandle := vfs.Request{Operation: vfs.OperationStat, Handle: opened.Handle}
	byPath := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: path})
	stat := service.Dispatch(testSession, byHandle)
	if stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindFile || stat.Size != 0 {
		t.Fatalf("empty file = %+v", stat)
	}
	// Both answers describe one file, so they carry one identity, and a
	// second file of the mount carries another.
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/other")})
	other := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/tmp/other")})
	listed := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/tmp")})
	if stat.Node == 0 || stat.Node != byPath.Node || other.Node == stat.Node || other.MountID != stat.MountID ||
		listed.Status != vfs.StatusOK || listed.Node != stat.Node && listed.Node != other.Node {
		t.Fatalf("identity: handle=%d path=%d other=%d listed=%d", stat.Node, byPath.Node, other.Node, listed.Node)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte("seven b")})
	if renamed := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRename, Path: path, Target: []byte("/tmp/renamed")}); renamed.Status != vfs.StatusOK {
		t.Fatalf("rename = %d", renamed.Status)
	}
	if stat := service.Dispatch(testSession, byHandle); stat.Status != vfs.StatusOK || stat.Size != 7 {
		t.Fatalf("after write and rename = %+v", stat)
	}
	if other := service.Dispatch(testSession+1, byHandle); other.Status != vfs.StatusNotFound {
		t.Fatalf("another session's stat = %d", other.Status)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if stale := service.Dispatch(testSession, byHandle); stale.Status != vfs.StatusNotFound {
		t.Fatalf("closed handle stat = %d", stale.Status)
	}
	if empty := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat}); empty.Status == vfs.StatusOK {
		t.Fatal("stat with neither path nor handle succeeded")
	}
}

// FAT32 names an open file by a cookie and a path by a directory location.
// The identity a stat reports must still be one value for both.
func TestServiceIdentityAgreesOnFAT32(t *testing.T) {
	service := startedService(t, testFAT32Device())
	path := []byte("/persist/identity.txt")
	if created := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: path}); created.Status != vfs.StatusOK {
		t.Fatalf("create = %d", created.Status)
	}
	before := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: path})
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.Write, Path: path})
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte("x")})
	byHandle := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Handle: opened.Handle})
	whileOpen := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: path})
	if opened.Status != vfs.StatusOK || byHandle.Status != vfs.StatusOK || before.Node == 0 ||
		byHandle.Node != before.Node || whileOpen.Node != before.Node || byHandle.Size != 1 || byHandle.MountID != before.MountID {
		t.Fatalf("identity: before=%d handle=%d while open=%d size=%d", before.Node, byHandle.Node, whileOpen.Node, byHandle.Size)
	}
	// A rename moves the directory entry, and with it the identity a stat
	// of the path reports. The open file follows.
	moved := []byte("/persist/identity-moved.txt")
	if renamed := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRename, Path: path, Target: moved}); renamed.Status != vfs.StatusOK {
		t.Fatalf("rename = %d", renamed.Status)
	}
	afterPath := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: moved})
	afterHandle := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Handle: opened.Handle})
	if afterPath.Status != vfs.StatusOK || afterHandle.Status != vfs.StatusOK || afterPath.Node == 0 || afterHandle.Node != afterPath.Node || afterHandle.Size != 1 {
		t.Fatalf("identity after rename: path=%d handle=%d (was %d)", afterPath.Node, afterHandle.Node, before.Node)
	}
}
