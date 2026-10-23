package vfs

import (
	"goos/kernel/ramfs"
	"testing"
)

func readPlanFile(t testing.TB) (*Namespace, Handle) {
	t.Helper()
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/read"), false) != StatusOK {
		t.Fatal("create")
	}
	file, status := namespace.Open(1, []byte("/read"), Read|Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	if _, status := namespace.Write(1, file, []byte("abcdef")); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.Seek(1, file, 0); status != StatusOK {
		t.Fatal(status)
	}
	return namespace, file
}

func TestReadPlanCommitsOnceOnOwner(t *testing.T) {
	namespace, file := readPlanFile(t)
	plan, status := namespace.BeginRead(1, file)
	if status != StatusOK {
		t.Fatal(status)
	}
	if _, status := namespace.BeginRead(1, file); status != StatusBusy {
		t.Fatalf("second begin = %d", status)
	}
	var data [2]byte
	count, status := plan.Read(data[:])
	if count != 2 || status != StatusOK || string(data[:]) != "ab" {
		t.Fatalf("read = %d %d %q", count, status, data)
	}
	entry, _ := namespace.file(1, file, 0)
	if entry.offset != 0 {
		t.Fatal("worker changed namespace cursor")
	}
	if status := namespace.FinishRead(plan, count, status); status != StatusOK || entry.offset != 2 {
		t.Fatalf("finish = %d offset = %d", status, entry.offset)
	}
	if status := namespace.FinishRead(plan, count, StatusOK); status != StatusInvalidArgument || entry.offset != 2 {
		t.Fatalf("duplicate finish = %d offset = %d", status, entry.offset)
	}
	if count, status := namespace.Read(1, file, data[:]); count != 2 || status != StatusOK || string(data[:]) != "cd" {
		t.Fatalf("next read = %d %d %q", count, status, data)
	}
}

func TestReadPlanErrorsDoNotAdvanceAndReleaseReservation(t *testing.T) {
	namespace, file := readPlanFile(t)
	for _, failure := range []Status{StatusBackendError, StatusTimeout, StatusPeerDied} {
		plan, status := namespace.BeginRead(1, file)
		if status != StatusOK {
			t.Fatal(status)
		}
		if status := namespace.FinishRead(plan, 2, failure); status != failure {
			t.Fatalf("finish = %d, want %d", status, failure)
		}
		entry, _ := namespace.file(1, file, 0)
		if entry.offset != 0 {
			t.Fatal("partial failure advanced cursor")
		}
	}
	plan, status := namespace.BeginRead(1, file)
	if status != StatusOK || namespace.FinishRead(plan, 0, StatusOK) != StatusOK {
		t.Fatal("zero read")
	}
	if status := namespace.FinishRead(plan, 0, StatusOK); status != StatusInvalidArgument {
		t.Fatalf("zero duplicate = %d", status)
	}
	plan, status = namespace.BeginRead(1, file)
	if status != StatusOK || namespace.FinishRead(plan, -1, StatusOK) != StatusBackendError {
		t.Fatal("negative count")
	}
	plan, status = namespace.BeginRead(1, file)
	if status != StatusOK || namespace.FinishRead(plan, 0, StatusOK) != StatusOK {
		t.Fatal("invalid result stranded reservation")
	}
}

func TestReadPlanValidatesOwnerRightsAndGeneration(t *testing.T) {
	namespace, file := readPlanFile(t)
	if _, status := namespace.BeginRead(2, file); status != StatusNotFound {
		t.Fatalf("wrong owner = %d", status)
	}
	writeOnly, status := namespace.Open(1, []byte("/read"), Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	if _, status := namespace.BeginRead(1, writeOnly); status != StatusPermissionDenied {
		t.Fatalf("write only = %d", status)
	}
	plan, status := namespace.BeginRead(1, file)
	if status != StatusOK {
		t.Fatal(status)
	}
	// The caller must drain a plan before close. A stale completion must still
	// never advance a later occupant if the caller violates that contract.
	if namespace.Close(1, file) != StatusOK {
		t.Fatal("close")
	}
	reused, status := namespace.Open(1, []byte("/read"), Read)
	if status != StatusOK || reused == file {
		t.Fatal("reuse")
	}
	if status := namespace.FinishRead(plan, 2, StatusOK); status != StatusNotFound {
		t.Fatalf("stale completion = %d", status)
	}
	entry, _ := namespace.file(1, reused, 0)
	if entry.offset != 0 {
		t.Fatal("stale completion advanced new handle")
	}
}

func TestReadPlanKeepsScopedPermissionChecks(t *testing.T) {
	namespace, file := readPlanFile(t)
	if status := namespace.InstallScope(0, []byte("/read"), false, Read); status != StatusOK {
		t.Fatal(status)
	}
	if _, status := namespace.BeginRead(1, file); status != StatusNotFound {
		t.Fatalf("revoked handle = %d", status)
	}
}

type invalidReadBackend struct {
	*ramfs.FileSystem
	count int
}

func (backend *invalidReadBackend) Read(_ ramfs.NodeID, _ uint64, _ []byte) (int, ramfs.Status) {
	return backend.count, ramfs.StatusOK
}

func TestReadPlanRejectsInvalidBackendCounts(t *testing.T) {
	fs := &ramfs.FileSystem{}
	root := fs.Init()
	fs.Create(root, []byte("read"))
	backend := &invalidReadBackend{FileSystem: fs}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, root) != StatusOK {
		t.Fatal("mount")
	}
	file, status := namespace.Open(1, []byte("/read"), Read)
	if status != StatusOK {
		t.Fatal(status)
	}
	for _, invalid := range []int{-1, 2} {
		backend.count = invalid
		plan, status := namespace.BeginRead(1, file)
		if status != StatusOK || !plan.BackendIs(backend) {
			t.Fatal("begin")
		}
		var target [1]byte
		count, status := plan.Read(target[:])
		if count != 0 || status != StatusBackendError {
			t.Fatalf("invalid count %d: %d %d", invalid, count, status)
		}
		if namespace.FinishRead(plan, count, status) != StatusBackendError {
			t.Fatal("finish invalid backend result")
		}
	}
}

func TestReadPlanShortReadAndEOF(t *testing.T) {
	namespace, file := readPlanFile(t)
	if namespace.Seek(1, file, 5) != StatusOK {
		t.Fatal("seek")
	}
	var target [4]byte
	for _, want := range []int{1, 0} {
		plan, status := namespace.BeginRead(1, file)
		if status != StatusOK {
			t.Fatal(status)
		}
		count, status := plan.Read(target[:])
		if count != want || status != StatusOK || namespace.FinishRead(plan, count, status) != StatusOK {
			t.Fatalf("read = %d %d, want %d", count, status, want)
		}
		entry, _ := namespace.file(1, file, 0)
		if entry.offset != 6 {
			t.Fatalf("offset = %d", entry.offset)
		}
	}
}

func TestReadPlanRejectsAnotherNamespace(t *testing.T) {
	namespace, file := readPlanFile(t)
	plan, status := namespace.BeginRead(1, file)
	if status != StatusOK {
		t.Fatal(status)
	}
	other := &Namespace{}
	if other.FinishRead(plan, 1, StatusOK) != StatusInvalidArgument {
		t.Fatal("foreign plan accepted")
	}
	if namespace.FinishRead(plan, 0, StatusOK) != StatusOK {
		t.Fatal("foreign finish consumed reservation")
	}
}
