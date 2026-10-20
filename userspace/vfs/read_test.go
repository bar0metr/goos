package vfsservice

import (
	"goos/kernel/vfs"
	"testing"
)

func TestServiceReadPlanLocalBackends(t *testing.T) {
	service := startedService(t, nil)
	if service.Dispatch(1, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/tmp/read")}).Status != vfs.StatusOK {
		t.Fatal("create")
	}
	for _, path := range []string{"/tmp/read", "/bootstrap/motd"} {
		opened := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte(path), Rights: vfs.Read})
		if opened.Status != vfs.StatusOK {
			t.Fatal(opened.Status)
		}
		plan, status := service.BeginRead(1, opened.Handle)
		if status != vfs.StatusOK || plan.Lane() != ReadLaneLocal {
			t.Fatalf("%s: begin = %d lane = %d", path, status, plan.Lane())
		}
		var target [4]byte
		count, status := plan.Read(target[:])
		if status != vfs.StatusOK || service.FinishRead(plan, count, status) != vfs.StatusOK {
			t.Fatal("finish")
		}
	}
}

func TestServiceReadPlanPersistentLane(t *testing.T) {
	service := startedService(t, testFAT32Device())
	writePersistent(t, service, "/home/read", "persistent")
	opened := service.Dispatch(1, vfs.Request{Operation: vfs.OperationOpen, Path: []byte("/home/read"), Rights: vfs.Read})
	if opened.Status != vfs.StatusOK {
		t.Fatal(opened.Status)
	}
	plan, status := service.BeginRead(1, opened.Handle)
	if status != vfs.StatusOK || plan.Lane() != ReadLanePersistent {
		t.Fatalf("begin = %d lane = %d", status, plan.Lane())
	}
	var target [4]byte
	count, status := plan.Read(target[:])
	if count != 4 || status != vfs.StatusOK || string(target[:]) != "pers" {
		t.Fatalf("read = %d %d %q", count, status, target)
	}
	if service.FinishRead(plan, count, status) != vfs.StatusOK {
		t.Fatal("finish")
	}
	if reply := service.Dispatch(1, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 6}); reply.Status != vfs.StatusOK || string(reply.Data) != "istent" {
		t.Fatalf("ordinary read after worker = %+v", reply)
	}
}
