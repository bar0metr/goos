package sys

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"testing"
	"unsafe"
)

func TestStringAndBytesShareStorage(t *testing.T) {
	data := []byte("shared")
	text := String(data)
	if text != "shared" || unsafe.StringData(text) != &data[0] {
		t.Fatalf("String copied or changed its input: %q", text)
	}
	view := Bytes(text)
	if string(view) != "shared" || &view[0] != &data[0] {
		t.Fatal("Bytes copied its input")
	}
	if String(nil) != "" || Bytes("") != nil {
		t.Fatal("empty values are not empty")
	}
}

// Arguments the wrappers can judge themselves are refused before the kernel
// is entered, with the status the kernel would give.
func TestWrappersRefuseArgumentsBeforeTheKernel(t *testing.T) {
	if _, status := WaitAny(nil, 1, 0); status != kcall.StatusInvalidArgument {
		t.Fatalf("nil interests = %d", status)
	}
	var interests [kcall.WaitAnyMaxObjects]Interest
	for _, count := range []int{-1, kcall.WaitAnyMaxObjects + 1} {
		if _, status := WaitAny(&interests, count, 0); status != kcall.StatusInvalidArgument {
			t.Fatalf("count %d = %d", count, status)
		}
	}
	for _, region := range [][2]uint64{
		{vm.UserSharedAddress, 0},
		{vm.UserSharedAddress + 1, vm.PageSize},
		{vm.PageSize, vm.PageSize},
		{vm.UserAddressLimit - vm.PageSize, 2 * vm.PageSize},
		{vm.UserSharedAddress, ^uint64(0)},
	} {
		if _, status := MemoryMap(handle.ID(1), region[0], region[1], true); status != kcall.StatusInvalidArgument {
			t.Fatalf("region %#x+%#x = %d", region[0], region[1], status)
		}
	}
}

// The interest record is the kernel's: two eight-byte words, handle first.
func TestInterestMatchesTheKernelRecord(t *testing.T) {
	var interest Interest
	if unsafe.Sizeof(interest) != uintptr(kcall.WaitAnyInterestSize) || unsafe.Offsetof(interest.Events) != 8 {
		t.Fatalf("interest layout = %d bytes, events at %d", unsafe.Sizeof(interest), unsafe.Offsetof(interest.Events))
	}
}

// A status the kernel returns reaches the caller unchanged, with no value
// invented for a call that did not succeed. On the host every call answers
// Unsupported.
func TestWrappersReturnTheKernelStatus(t *testing.T) {
	var buffer [8]byte
	if status := IPCSend(handle.ID(1), buffer[:]); status != kcall.StatusUnsupported {
		t.Fatalf("IPCSend = %d", status)
	}
	if count, status := StreamRead(handle.ID(1), buffer[:]); status != kcall.StatusUnsupported || count != 0 {
		t.Fatalf("StreamRead = %d, %d", count, status)
	}
	if first, second, status := ChannelCreate(); status != kcall.StatusUnsupported || first != 0 || second != 0 {
		t.Fatalf("ChannelCreate = %#x, %#x, %d", first, second, status)
	}
	if code, status := ProcessWait(handle.ID(1), 0); status != kcall.StatusUnsupported || code != 0 {
		t.Fatalf("ProcessWait = %d, %d", code, status)
	}
	if image, status := ImageMap(handle.ID(1)); status != kcall.StatusUnsupported || image != nil {
		t.Fatalf("ImageMap = %d bytes, %d", len(image), status)
	}
	if region, status := DMAAllocate(handle.ID(1), 0x401c0000, 16384); status != kcall.StatusUnsupported || region.Memory != nil {
		t.Fatalf("DMAAllocate = %#v, %d", region, status)
	}
}
