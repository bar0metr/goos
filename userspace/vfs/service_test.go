package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"testing"
)

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
