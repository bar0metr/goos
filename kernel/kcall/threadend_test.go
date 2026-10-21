package kcall

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/sched"
)

func TestThreadEndInterestNamesNoHandleOrTask(t *testing.T) {
	// The tag bit must lie above every handle index and task slot, so the
	// encoding never collides with a real handle and decodes to the ID.
	if handle.MaxHandles >= WaitAnyThreadEnd || sched.MaxTasks >= WaitAnyThreadEnd {
		t.Fatal("the thread-end tag overlaps handle or task indices")
	}
	thread, _ := sched.NewTaskID(sched.MaxTasks-1, ^uint32(0))
	encoded := ThreadEndInterest(uint64(thread))
	if encoded == WaitAnyThread {
		t.Fatal("a thread-end interest is the wake-flag interest")
	}
	if got, ok := ThreadEndTarget(encoded); !ok || got != uint64(thread) {
		t.Fatalf("decode = %#x, %v", got, ok)
	}
	for _, other := range []uint64{0, 1, uint64(1)<<32 | 1, uint64(handle.MaxHandles), WaitAnyThread} {
		if _, ok := ThreadEndTarget(other); ok {
			t.Fatalf("%#x decoded as a thread end", other)
		}
	}
}

func TestJoinFlagsAreSeparateModes(t *testing.T) {
	for flags := uint64(0); flags < 32; flags++ {
		want := flags == 0 || flags == JoinPoll || flags == JoinClaim || flags == JoinCollect || flags == JoinRelease
		if ValidJoinFlags(flags) != want {
			t.Fatalf("join flags %d valid=%v want=%v", flags, ValidJoinFlags(flags), want)
		}
	}
}

func TestIRQWaitFlagsKeepPollAndBlockingSeparate(t *testing.T) {
	for flags := uint64(0); flags < 8; flags++ {
		want := flags == 0 || flags == IRQWaitPoll
		if ValidIRQWaitFlags(flags) != want {
			t.Fatalf("IRQ wait flags %d valid=%v want=%v", flags, ValidIRQWaitFlags(flags), want)
		}
	}
}
