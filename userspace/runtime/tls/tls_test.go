package tls

import (
	"testing"

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
	defer func() {
		if recover() == nil {
			t.Fatal("a stack pointer below the thread area was accepted")
		}
	}()
	SlotOf(0x1000)
}
