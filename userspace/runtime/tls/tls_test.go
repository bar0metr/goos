package tls

import (
	"testing"
	"unsafe"

	"goos/kernel/kcall"
)

func TestSlotOfFollowsTheStackLayout(t *testing.T) {
	if SlotOf(uintptr(kcall.ThreadAreaTop)+64) != 0 {
		t.Fatal("the main stack is not slot 0")
	}
	for slot := uint64(1); slot < Slots; slot++ {
		if got := SlotOf(uintptr(kcall.ThreadStackTop(slot) - 8)); got != int(slot) {
			t.Fatalf("slot %d read as %d", slot, got)
		}
	}
	if SlotOf(0x1000) != Slots {
		t.Fatal("a stack pointer below the thread area named a slot")
	}
}

// TestBlockLayout pins the offsets the assembly uses.
func TestBlockLayout(t *testing.T) {
	var b Block
	offsets := []uintptr{
		unsafe.Offsetof(b.Self), unsafe.Offsetof(b.Slot), unsafe.Offsetof(b.Magic), unsafe.Offsetof(b.M),
		unsafe.Offsetof(b.Pins), unsafe.Offsetof(b.G), unsafe.Offsetof(b.ID), unsafe.Offsetof(b.Task),
	}
	for index, offset := range offsets {
		if offset != uintptr(index*8) {
			t.Fatalf("field %d at %d", index, offset)
		}
	}
	if unsafe.Sizeof(b) != 64 {
		t.Fatalf("block is %d bytes", unsafe.Sizeof(b))
	}
}
