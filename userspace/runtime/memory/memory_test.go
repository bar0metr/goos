package memory

import (
	"testing"
	"unsafe"
)

func TestSliceAddressAndTornBytes(t *testing.T) {
	if SliceAddress(nil) != 0 {
		t.Fatal("a nil slice has an array address")
	}
	buffer := []byte{1, 2, 3}
	if SliceAddress(buffer) != uintptr(unsafe.Pointer(&buffer[0])) {
		t.Fatal("wrong array address")
	}
	torn := Bytes(0, 4096)
	if len(torn) != 4096 || cap(torn) != 4096 || SliceAddress(torn) != 0 {
		t.Fatalf("torn header len=%d cap=%d address=%#x", len(torn), cap(torn), SliceAddress(torn))
	}
}

// goprobe's gcstress reads cell.value at offset 8 through Word; a cell keeps
// a pointer first.
func TestWordReadsAPointerFollowedValue(t *testing.T) {
	type cell struct {
		next  *cell
		value uint64
	}
	value := &cell{value: 0x5a3c96e1c3a5f00f}
	if offset := unsafe.Offsetof(value.value); offset != 8 {
		t.Fatalf("value at offset %d", offset)
	}
	if got := Word(uintptr(unsafe.Pointer(value)) + 8); got != value.value {
		t.Fatalf("Word read %#x", got)
	}
}
