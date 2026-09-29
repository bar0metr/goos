package linux

import (
	"testing"

	"goos/kernel/handle"
)

func TestFDTableInstallsOnlyCompatibilityIntegers(t *testing.T) {
	var table FDTable
	if !table.Initialize(handle.ID(0x100000001), handle.ID(0x200000002), handle.ID(0x300000003)) {
		t.Fatal("initialize failed")
	}
	cases := []struct {
		fd     int
		kind   FDKind
		object uint64
	}{{0, FDStreamReader, 0x100000001}, {1, FDStreamWriter, 0x200000002}, {2, FDStreamWriter, 0x300000003}}
	for _, test := range cases {
		description, found := table.Lookup(test.fd)
		if !found || description.Kind != test.kind || description.Object != test.object || description.References != 1 {
			t.Fatalf("fd %d = %#v found=%v", test.fd, description, found)
		}
	}
	if _, found := table.Lookup(int(0x100000001)); found {
		t.Fatal("native handle accepted as Linux fd")
	}
}

func TestFDTableIsBoundedAndReturnsEMFILEBoundary(t *testing.T) {
	var table FDTable
	if !table.Initialize(1, 2, 3) {
		t.Fatal("initialize failed")
	}
	for fd := 3; fd < LinuxMaxFD; fd++ {
		allocated, ok := table.Allocate(FileDescription{Kind: FDVFSFile, Object: uint64(fd + 10)}, false)
		if !ok || allocated != fd {
			t.Fatalf("allocation %d = %d ok=%v", fd, allocated, ok)
		}
	}
	if fd, ok := table.Allocate(FileDescription{Kind: FDVFSFile, Object: 999}, false); ok || fd != -1 {
		t.Fatalf("over-capacity allocation = %d ok=%v", fd, ok)
	}
}

func TestFDTableSharesDescriptionAndReleasesOnce(t *testing.T) {
	var table FDTable
	if !table.Initialize(1, 2, 3) {
		t.Fatal("initialize failed")
	}
	duplicate, ok := table.Duplicate(1, 3, true)
	if !ok || duplicate != 3 {
		t.Fatalf("duplicate = %d ok=%v", duplicate, ok)
	}
	if released, last, ok := table.Close(1); !ok || last || released.Kind != FDInvalid {
		t.Fatalf("first close = %#v last=%v ok=%v", released, last, ok)
	}
	description, found := table.Lookup(3)
	if !found || description.References != 1 || description.Offset != 0 {
		t.Fatalf("shared description = %#v found=%v", description, found)
	}
	if released, last, ok := table.Close(3); !ok || !last || released.Kind != FDStreamWriter {
		t.Fatalf("last close = %#v last=%v ok=%v", released, last, ok)
	}
}

func TestFDTableCloseAllReleasesDescriptionsOnce(t *testing.T) {
	var table FDTable
	if !table.Initialize(1, 2, 3) {
		t.Fatal("initialize failed")
	}
	if _, ok := table.Duplicate(1, 3, false); !ok {
		t.Fatal("duplicate failed")
	}
	released := 0
	table.CloseAll(func(FileDescription) { released++ })
	if released != 3 {
		t.Fatalf("released descriptions = %d", released)
	}
	for fd := 0; fd < LinuxMaxFD; fd++ {
		if _, found := table.Lookup(fd); found {
			t.Fatalf("fd %d survived teardown", fd)
		}
	}
}
