package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"testing"
)

type testBlockDevice struct{ data []byte }

func (device *testBlockDevice) ReadAt(offset uint64, target []byte) bool {
	return offset+uint64(len(target)) <= uint64(len(device.data)) && copy(target, device.data[offset:]) == len(target)
}
func (device *testBlockDevice) WriteAt(offset uint64, data []byte) bool {
	return offset+uint64(len(data)) <= uint64(len(device.data)) && copy(device.data[offset:], data) == len(data)
}
func (*testBlockDevice) Flush() bool { return true }

func testFAT32Device() *testBlockDevice {
	device := &testBlockDevice{data: make([]byte, 4*1024*1024)}
	boot := device.data[:512]
	boot[11], boot[12], boot[13] = 0, 2, 1
	boot[14], boot[15], boot[16] = 32, 0, 1
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
	var service Service
	if !service.Init() {
		t.Fatal("service init failed")
	}
	stat := service.Dispatch(vfs.Request{Operation: vfs.OperationStat, Path: []byte("/system")})
	if stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		t.Fatalf("stat = %+v", stat)
	}
	opened := service.Dispatch(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/system/motd")})
	if opened.Status != vfs.StatusOK || opened.Handle == 0 {
		t.Fatalf("open = %+v", opened)
	}
	read := service.Dispatch(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != "Welcome to GoOS\n" {
		t.Fatalf("read = %d, %q", read.Status, read.Data)
	}
	if closed := service.Dispatch(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle}); closed.Status != vfs.StatusOK {
		t.Fatalf("close = %d", closed.Status)
	}
}

func TestServiceReadDirIsBounded(t *testing.T) {
	var service Service
	if !service.Init() {
		t.Fatal("service init failed")
	}
	first := service.Dispatch(vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/"), Offset: 0})
	if first.Status != vfs.StatusOK || first.Cursor == 0 || len(first.Name) == 0 {
		t.Fatalf("readdir = %+v", first)
	}
}

func TestServicePersistentFileLifecycle(t *testing.T) {
	var service Service
	if !service.Init() || !service.MountPersistent(testFAT32Device()) {
		t.Fatal("service mount failed")
	}
	if reply := service.Dispatch(vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/state.txt")}); reply.Status != vfs.StatusOK {
		t.Fatalf("create = %d", reply.Status)
	}
	opened := service.Dispatch(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte("/persist/state.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open write = %d", opened.Status)
	}
	payload := []byte("Welcome to GoOS\n")
	if reply := service.Dispatch(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: payload}); reply.Status != vfs.StatusOK || reply.Size != uint64(len(payload)) {
		t.Fatalf("write = %+v", reply)
	}
	service.Dispatch(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	opened = service.Dispatch(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/state.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open read = %d", opened.Status)
	}
	read := service.Dispatch(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != string(payload) {
		t.Fatalf("read = %d, %q", read.Status, read.Data)
	}
}

func TestServicePersistentSubdirectoryLifecycle(t *testing.T) {
	device := testFAT32Device()
	service := &Service{}
	if !service.Init() || !service.MountPersistent(device) {
		t.Fatal("service mount failed")
	}
	dispatch := func(request vfs.Request) vfs.Reply {
		t.Helper()
		reply := service.Dispatch(request)
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
	if end := service.Dispatch(vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/persist/demo"), Offset: uint64(listed.Cursor)}); end.Status != vfs.StatusNotFound {
		t.Fatalf("readdir end = %d", end.Status)
	}

	service = &Service{}
	if !service.Init() || !service.MountPersistent(device) {
		t.Fatal("remount failed")
	}
	opened = dispatch(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if read := dispatch(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64}); string(read.Data) != "persistent\n" {
		t.Fatalf("read = %q", read.Data)
	}
	dispatch(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if reply := service.Dispatch(vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/demo")}); reply.Status == vfs.StatusOK {
		t.Fatal("removed non-empty directory")
	}
	dispatch(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
	dispatch(vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/demo")})
	if reply := service.Dispatch(vfs.Request{Operation: vfs.OperationStat, Path: []byte("/persist/demo")}); reply.Status != vfs.StatusNotFound {
		t.Fatalf("stat removed = %d", reply.Status)
	}
}
