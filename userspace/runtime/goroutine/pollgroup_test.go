package goroutine

import (
	"testing"

	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

func TestPollGroupPreservesOriginalIdentityAndMask(t *testing.T) {
	var snapshot [MaxWaiters]registration
	snapshot[0] = registration{index: 1, gen: 2, seq: 3, bit: 2, handle: 7, events: kcall.WaitReadable}
	// A different goroutine may watch exactly the same endpoint.
	snapshot[1] = registration{index: 2, gen: 2, seq: 3, bit: 0, handle: 7, events: kcall.WaitPeerClosed}
	snapshot[2] = registration{index: 1, gen: 2, seq: 3, bit: 0, handle: 9, events: kcall.WaitReadable}
	// Reused slot and subsequent wait are not part of the original group.
	snapshot[3] = registration{index: 1, gen: 3, seq: 3, bit: 1, handle: 10}
	snapshot[4] = registration{index: 1, gen: 2, seq: 4, bit: 1, handle: 11}
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	var bits [MaxWaiters]uint8
	members, count := waitGroup(&snapshot, 0, 5, &interests, &bits)
	if members != 5 || count != 2 || interests[0].Handle != 7 || interests[1].Handle != 9 {
		t.Fatalf("members=%b count=%d interests=%+v", members, count, interests[:count])
	}
	if got := groupReadyMask(3, &bits, count); got != 5 {
		t.Fatalf("combined original ready bits=%b want 101", got)
	}
	if got := groupReadyMask(2, &bits, count); got != 1 {
		t.Fatalf("second original ready bit=%b want 1", got)
	}
	members, count = waitGroup(&snapshot, 1, 5, &interests, &bits)
	if members != 2 || count != 1 || interests[0].Events != kcall.WaitPeerClosed {
		t.Fatal("overlapping event subscriptions were merged")
	}
}

func TestPollGroupRetainsAliasesForKernelValidation(t *testing.T) {
	var snapshot [MaxWaiters]registration
	for index := 0; index < MaxWaiters; index++ {
		snapshot[index] = registration{index: 1, gen: 1, seq: 1, bit: uint8(index), handle: 7, events: kcall.WaitReadable}
	}
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	var bits [MaxWaiters]uint8
	members, count := waitGroup(&snapshot, 0, MaxWaiters, &interests, &bits)
	if count != MaxWaiters || members != (uint64(1)<<MaxWaiters)-1 {
		t.Fatal("maximum group truncated")
	}
	if got := groupReadyMask(members, &bits, count); got != members {
		t.Fatal("maximum ready mask truncated")
	}
	for _, interest := range interests[:count] {
		if interest.Handle != 7 {
			t.Fatal("duplicate interest removed before kernel validation")
		}
	}
}
