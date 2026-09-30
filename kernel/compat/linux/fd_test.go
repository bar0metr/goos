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

func TestDuplicateAtAndDescriptorFlags(t *testing.T) {
	var table FDTable
	if !table.Initialize(1, 2, 3) {
		t.Fatal("initialize failed")
	}
	if fd, ok := table.Duplicate(9, 0, false); ok || fd != -2 {
		t.Fatalf("duplicate of a closed descriptor = %d", fd)
	}
	if !table.DuplicateAt(1, 10, true) || table.DuplicateAt(1, 10, false) || table.DuplicateAt(40, 11, false) ||
		table.DuplicateAt(1, LinuxMaxFD, false) || table.DuplicateAt(1, -1, false) {
		t.Fatal("DuplicateAt did not require an open source and a free target")
	}
	first, _ := table.DescriptionIndex(1)
	second, _ := table.DescriptionIndex(10)
	if first != second {
		t.Fatal("the duplicate has its own description")
	}
	// The flag belongs to the integer.
	if flag, ok := table.CloseOnExec(10); !ok || !flag {
		t.Fatal("close-on-exec was not set on the duplicate")
	}
	if flag, ok := table.CloseOnExec(1); !ok || flag {
		t.Fatal("close-on-exec leaked to the original")
	}
	if !table.SetCloseOnExec(1, true) || table.SetCloseOnExec(40, true) {
		t.Fatal("SetCloseOnExec accepted a closed descriptor")
	}
	if _, ok := table.CloseOnExec(40); ok {
		t.Fatal("a closed descriptor has a flag")
	}
	if _, last, ok := table.Close(1); !ok || last {
		t.Fatal("closing one of two descriptors released the description")
	}
	if _, last, ok := table.Close(10); !ok || !last {
		t.Fatal("closing the last descriptor kept the description")
	}
	for fd := 3; fd < LinuxMaxFD; fd++ {
		table.Duplicate(0, fd, false)
	}
	if fd, ok := table.Duplicate(0, 0, false); !ok || fd != 1 {
		t.Fatalf("lowest free descriptor = %d", fd)
	}
	if table.DuplicateAt(0, 10, false) {
		t.Fatal("DuplicateAt replaced an open target")
	}
	if fd, ok := table.Duplicate(0, 0, false); ok || fd != -1 {
		t.Fatalf("duplicate into a full table = %d", fd)
	}
}
