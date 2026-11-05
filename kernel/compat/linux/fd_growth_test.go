package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/objects"
	"goos/kernel/process"
)

// countFDPages installs storage that counts the pages tables hold, with
// limit pages at most when limit is not zero.
func countFDPages(t *testing.T, limit int) *objects.Limited[FDChunk] {
	t.Helper()
	storage := &objects.Limited[FDChunk]{Limit: limit}
	SetFDStorage(storage)
	t.Cleanup(func() { SetFDStorage(nil) })
	return storage
}

func fdOwner(t *testing.T, id process.ID) *process.Process {
	t.Helper()
	owner, ok := process.New(id, process.ABILinuxX8664, process.AddressSpace{Root: uint64(id) << 12})
	if !ok {
		t.Fatal("process.New failed")
	}
	return owner
}

func chargedPages(owner *process.Process) uint32 {
	used, _ := owner.Quota()
	return used[process.ResourcePages]
}

func TestFDTableGrowsInPagesChargedToItsOwner(t *testing.T) {
	resetFDTest()
	pages := countFDPages(t, 0)
	owner := fdOwner(t, 30)
	var table FDTable
	if !table.Initialize(1, 1, 2, 3) || !table.SetOwner(owner) {
		t.Fatal("initialize failed")
	}
	if soft, hard := table.Limits(); soft != DefaultFDSoftLimit || hard != DefaultFDHardLimit {
		t.Fatalf("limits = %d/%d", soft, hard)
	}
	for fd := 3; fd < fdInlineEntries; fd++ {
		if _, errno := table.Allocate(FileDescription{Kind: FDSynthetic, Object: 1}, false); errno != 0 {
			t.Fatalf("allocate %d errno=%d", fd, errno)
		}
	}
	if pages.Live != 0 || chargedPages(owner) != 0 || table.Capacity() != fdInlineEntries {
		t.Fatalf("a table of %d descriptors took %d pages", fdInlineEntries, pages.Live)
	}
	if fd, errno := table.Allocate(FileDescription{Kind: FDSynthetic, Object: 1}, false); errno != 0 || fd != fdInlineEntries {
		t.Fatalf("first paged descriptor = %d errno=%d", fd, errno)
	}
	if pages.Live != 1 || chargedPages(owner) != 1 || table.Capacity() != fdInlineEntries+FDChunkEntries {
		t.Fatalf("pages=%d charged=%d capacity=%d", pages.Live, chargedPages(owner), table.Capacity())
	}
	for {
		if _, errno := table.Allocate(FileDescription{Kind: FDSynthetic, Object: 1}, false); errno != 0 {
			if errno != abi.EMFILE {
				t.Fatalf("filling errno = %d", errno)
			}
			break
		}
	}
	if pages.Live != 1 {
		t.Fatalf("a table at the default soft limit holds %d pages", pages.Live)
	}
	if _, errno := table.Duplicate(0, 0, false); errno != abi.EMFILE {
		t.Fatalf("F_DUPFD on a full table errno = %d", errno)
	}
	table.CloseAll(nil)
	if pages.Live != 0 || chargedPages(owner) != 0 {
		t.Fatalf("after CloseAll: pages=%d charged=%d", pages.Live, chargedPages(owner))
	}
}

func TestFDTableGrowthFailureIsENOMEM(t *testing.T) {
	resetFDTest()
	countFDPages(t, 1)
	var first, second FDTable
	if !first.Initialize(1, 1, 2, 3) || !second.Initialize(2, 1, 2, 3) {
		t.Fatal("initialize failed")
	}
	if errno := first.DuplicateAt(1, 1000, false); errno != 0 {
		t.Fatalf("first table growth errno = %d", errno)
	}
	if errno := second.DuplicateAt(1, 1000, false); errno != abi.ENOMEM {
		t.Fatalf("growth without a page errno = %d", errno)
	}
	if _, found := second.Lookup(1000); found || second.Capacity() != fdInlineEntries {
		t.Fatal("a failed growth left a descriptor or a page")
	}
	if _, errno := second.Duplicate(1, 64, false); errno != abi.ENOMEM {
		t.Fatalf("F_DUPFD without a page errno = %d", errno)
	}
	first.CloseAll(nil)
	second.CloseAll(nil)

	// A quota with no pages left refuses the charge before any page is taken.
	resetFDTest()
	pages := countFDPages(t, 0)
	owner := fdOwner(t, 31)
	for owner.Charge(process.ResourcePages, 1) {
	}
	var table FDTable
	if !table.Initialize(3, 1, 2, 3) || !table.SetOwner(owner) {
		t.Fatal("initialize failed")
	}
	if errno := table.DuplicateAt(1, 1000, false); errno != abi.ENOMEM || pages.Live != 0 {
		t.Fatalf("over-quota growth errno=%d pages=%d", errno, pages.Live)
	}
}

func TestDup2ReachesHighDescriptorsWithinTheSoftLimit(t *testing.T) {
	resetFDTest()
	pages := countFDPages(t, 0)
	var table FDTable
	if !table.Initialize(1, 1, 2, 3) {
		t.Fatal("initialize failed")
	}
	if errno := table.DuplicateAt(1, 1000, true); errno != 0 {
		t.Fatalf("dup2 to 1000 errno = %d", errno)
	}
	low, _ := table.DescriptionIndex(1)
	high, found := table.DescriptionIndex(1000)
	if !found || low != high || pages.Live != 1 {
		t.Fatalf("fd 1000 found=%v shares=%v pages=%d", found, low == high, pages.Live)
	}
	if flag, _ := table.CloseOnExec(1000); !flag {
		t.Fatal("dup3 O_CLOEXEC lost")
	}
	if fd, errno := table.Duplicate(1, 0, false); errno != 0 || fd != 3 {
		t.Fatalf("lowest free after a high dup2 = %d errno=%d", fd, errno)
	}
	if fd, errno := table.Duplicate(1, 999, false); errno != 0 || fd != 999 {
		t.Fatalf("F_DUPFD 999 = %d errno=%d", fd, errno)
	}
	if fd, errno := table.Duplicate(1, 1000, false); errno != 0 || fd != 1001 {
		t.Fatalf("F_DUPFD 1000 = %d errno=%d", fd, errno)
	}
	if errno := table.DuplicateAt(1, DefaultFDSoftLimit, false); errno != abi.EBADF {
		t.Fatalf("dup2 at the soft limit errno = %d", errno)
	}

	// The soft limit moves up to the hard one; the hard one only comes down.
	if errno := table.SetLimits(4096, 8192); errno != abi.EPERM {
		t.Fatalf("raising the hard limit errno = %d", errno)
	}
	if errno := table.SetLimits(4097, 4096); errno != abi.EINVAL {
		t.Fatalf("soft above hard errno = %d", errno)
	}
	if errno := table.SetLimits(LinuxFDCeiling, LinuxFDCeiling); errno != 0 {
		t.Fatalf("raising the soft limit errno = %d", errno)
	}
	if errno := table.DuplicateAt(1, LinuxFDCeiling-1, false); errno != 0 {
		t.Fatalf("dup2 to the last descriptor errno = %d", errno)
	}
	if pages.Live != fdChunkSlots || table.Capacity() < LinuxFDCeiling {
		t.Fatalf("pages=%d capacity=%d", pages.Live, table.Capacity())
	}
	if errno := table.DuplicateAt(1, LinuxFDCeiling, false); errno != abi.EBADF {
		t.Fatalf("dup2 past the ceiling errno = %d", errno)
	}
	// A lowered soft limit keeps open descriptors and refuses new ones.
	if errno := table.SetLimits(10, 10); errno != 0 {
		t.Fatalf("lowering errno = %d", errno)
	}
	if _, found := table.Lookup(1000); !found {
		t.Fatal("lowering the limit closed a descriptor")
	}
	if errno := table.SetLimits(20, 20); errno != abi.EPERM {
		t.Fatalf("raising a lowered hard limit errno = %d", errno)
	}
	table.CloseAll(nil)
	if pages.Live != 0 {
		t.Fatalf("pages after CloseAll = %d", pages.Live)
	}
}

func TestReleasedCollectsAFullTable(t *testing.T) {
	resetFDTest()
	countFDPages(t, 0)
	var table FDTable
	if !table.Initialize(1, 1, 2, 3) {
		t.Fatal("initialize failed")
	}
	for fd := 3; fd < DefaultFDSoftLimit; fd++ {
		if _, errno := table.Allocate(FileDescription{Kind: FDSynthetic, Object: uint64(fd)}, false); errno != 0 {
			t.Fatalf("allocate %d errno=%d", fd, errno)
		}
	}
	var released Released
	table.CloseAll(&released)
	if released.Len() != DefaultFDSoftLimit {
		t.Fatalf("released %d descriptions", released.Len())
	}
	for index := 3; index < released.Len(); index++ {
		if got := released.At(index); got.Object != uint64(index) {
			t.Fatalf("released %d = %#v", index, got)
		}
	}
	// Reading out of order walks the list again.
	if got := released.At(10); got.Object != 10 {
		t.Fatalf("released 10 = %#v", got)
	}
	released.Reset()
	if descriptions, references := OpenDescriptionCount(); descriptions != 0 || references != 0 {
		t.Fatalf("after Reset: %d descriptions, %d references", descriptions, references)
	}
}

func TestForkCopiesHighDescriptorsAndChargesTheChild(t *testing.T) {
	resetCompatibilityProcesses()
	pages := countFDPages(t, 0)
	parentOwner, childOwner := fdOwner(t, 40), fdOwner(t, 41)
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0xa000, 40, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) ||
		!SetFDOwner(0xa000, parentOwner) {
		t.Fatal("install failed")
	}
	parent, _ := FindProcess(0xa000)
	file, errno := AllocateFD(0xa000, FileDescription{Kind: FDVFSFile, Object: 77}, true)
	if errno != 0 || parent.FDs.DuplicateAt(file, 1000, false) != 0 {
		t.Fatalf("parent setup errno=%d", errno)
	}
	record, registered := registerChildForTest(41, parent.PID)
	if !registered || !ForkProcess(0xa000, 0xb000, record, 9, childOwner) {
		t.Fatal("fork failed")
	}
	child, _ := FindProcess(0xb000)
	if pages.Live != 2 || chargedPages(parentOwner) != 1 || chargedPages(childOwner) != 1 {
		t.Fatalf("pages=%d parent=%d child=%d", pages.Live, chargedPages(parentOwner), chargedPages(childOwner))
	}
	description, found := child.FDs.Lookup(1000)
	if !found || description.Object != 77 || description.References != 4 {
		t.Fatalf("child fd 1000 = %#v found=%v", description, found)
	}
	if flag, _ := child.FDs.CloseOnExec(file); !flag {
		t.Fatal("child lost close-on-exec")
	}
	if flag, _ := child.FDs.CloseOnExec(1000); flag {
		t.Fatal("child gained close-on-exec")
	}
	if soft, hard := child.FDs.Limits(); soft != DefaultFDSoftLimit || hard != DefaultFDHardLimit {
		t.Fatalf("child limits = %d/%d", soft, hard)
	}
	if fd, errno := AllocateFD(0xb000, FileDescription{Kind: FDSynthetic, Object: 5}, false); errno != 0 || fd != 4 {
		t.Fatalf("child's lowest free = %d errno=%d", fd, errno)
	}
	// The child's exec sees both its integers for the file marked only at
	// file, so the file stays open.
	var handles [4]uint64
	if count := child.CloseOnExecVFS(handles[:]); count != 0 {
		t.Fatalf("close-on-exec handles = %v", handles[:count])
	}
	var released Released
	if !RemoveProcess(0xb000, &released) {
		t.Fatal("child removal failed")
	}
	if released.Len() != 1 || released.At(0).Object != 5 {
		t.Fatalf("child removal released %d", released.Len())
	}
	released.Reset()
	if pages.Live != 1 || chargedPages(childOwner) != 0 {
		t.Fatalf("after child removal: pages=%d child=%d", pages.Live, chargedPages(childOwner))
	}
	if description, _ := parent.FDs.Lookup(1000); description.References != 2 {
		t.Fatalf("parent references = %d", description.References)
	}
	if !RemoveProcess(0xa000, &released) || pages.Live != 0 || chargedPages(parentOwner) != 0 {
		t.Fatalf("after parent removal: pages=%d parent=%d", pages.Live, chargedPages(parentOwner))
	}
	released.Reset()
	resetCompatibilityProcesses()
}

func TestForkFailsWholeWhenTheChildCannotPayForItsTable(t *testing.T) {
	resetCompatibilityProcesses()
	pages := countFDPages(t, 0)
	childOwner := fdOwner(t, 51)
	for childOwner.Charge(process.ResourcePages, 1) {
	}
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0xa000, 50, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	parent, _ := FindProcess(0xa000)
	if parent.FDs.DuplicateAt(1, 2000, false) != abi.EBADF || parent.FDs.SetLimits(4096, 4096) != 0 || parent.FDs.DuplicateAt(1, 2000, false) != 0 {
		t.Fatal("parent setup failed")
	}
	descriptions, references := OpenDescriptionCount()
	record, registered := registerChildForTest(51, parent.PID)
	if !registered || ForkProcess(0xa000, 0xb000, record, 9, childOwner) {
		t.Fatal("fork succeeded without pages for the child's table")
	}
	if nowDescriptions, nowReferences := OpenDescriptionCount(); nowDescriptions != descriptions || nowReferences != references {
		t.Fatalf("descriptions %d/%d, want %d/%d", nowDescriptions, nowReferences, descriptions, references)
	}
	if _, found := FindProcess(0xb000); found || pages.Live != 2 {
		t.Fatalf("failed fork left a child or pages=%d", pages.Live)
	}
	resetCompatibilityProcesses()
}
