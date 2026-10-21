package sys

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
)

func TestProcessWaitHandlerDispatchAndPolling(t *testing.T) {
	defer InstallProcessWaitHandler(nil)
	wantCode, wantStatus := ProcessWait(7, 0)
	calls := 0
	handler := ProcessWaitHandler(func(process handle.ID, timeout uint64) (uint64, kcall.Status, bool) {
		calls++
		if process != 7 || timeout != 17 {
			t.Fatalf("process handler(%d, %d)", process, timeout)
		}
		// The adapter must be able to make its raw zero-time probe without
		// re-entering itself or holding the wrapper's scratch descriptor.
		if code, status := ProcessWait(process, 0); code != wantCode || status != wantStatus {
			t.Fatalf("raw probe = %d, %v; want %d, %v", code, status, wantCode, wantStatus)
		}
		return 42, kcall.StatusOK, true
	})
	InstallProcessWaitHandler(&handler)
	if code, status := ProcessWait(7, 17); code != 42 || status != kcall.StatusOK || calls != 1 {
		t.Fatalf("process wait = %d, %v; calls=%d", code, status, calls)
	}
	if code, status := ProcessWait(7, 0); code != wantCode || status != wantStatus || calls != 1 {
		t.Fatalf("poll = %d, %v; calls=%d", code, status, calls)
	}
	InstallProcessWaitHandler(nil)
	wantCode, wantStatus = ProcessWait(7, 17)
	handler = func(handle.ID, uint64) (uint64, kcall.Status, bool) {
		return 99, kcall.StatusOK, false
	}
	InstallProcessWaitHandler(&handler)
	if code, status := ProcessWait(7, 17); code != wantCode || status != wantStatus {
		t.Fatalf("declined process wait = %d, %v; want native %d, %v", code, status, wantCode, wantStatus)
	}
}

func TestReceiveHandlerDispatchAndPolling(t *testing.T) {
	defer InstallReceiveHandler(nil)
	var buffer [ipc.WireMaxSize]byte
	calls := 0
	handler := ReceiveHandler(func(channel handle.ID, got []byte, timeout uint64) (kcall.Status, bool) {
		calls++
		if channel != 7 || timeout != 17 || len(got) != len(buffer) || &got[0] != &buffer[0] {
			t.Fatal("IPC receive handler arguments changed")
		}
		got[0] = 42
		return kcall.StatusTimeout, true
	})
	InstallReceiveHandler(&handler)
	if status := IPCWaitReceive(7, buffer[:], 17); status != kcall.StatusTimeout || calls != 1 || buffer[0] != 42 {
		t.Fatalf("IPC receive status=%v calls=%d byte=%d", status, calls, buffer[0])
	}
	IPCWaitReceive(7, buffer[:], 0)
	if calls != 1 {
		t.Fatal("zero-time IPC receive called the scheduler")
	}
	InstallReceiveHandler(nil)
	want := IPCWaitReceive(7, buffer[:], 17)
	handler = func(handle.ID, []byte, uint64) (kcall.Status, bool) {
		return kcall.StatusOK, false
	}
	InstallReceiveHandler(&handler)
	if status := IPCWaitReceive(7, buffer[:], 17); status != want {
		t.Fatalf("declined IPC receive=%v want native %v", status, want)
	}
}

func TestWaitHandlerDispatch(t *testing.T) {
	defer InstallWaitHandler(nil)
	var interests [kcall.WaitAnyMaxObjects]Interest
	calls := 0
	handler := WaitHandler(func(got *[kcall.WaitAnyMaxObjects]Interest, count int, timeout uint64) (uint64, kcall.Status, bool) {
		calls++
		if got != &interests || count != 2 || timeout != 17 {
			t.Fatalf("handler arguments: %p %d %d", got, count, timeout)
		}
		return 2, kcall.StatusOK, true
	})
	InstallWaitHandler(&handler)
	mask, status := WaitAny(&interests, 2, 17)
	if mask != 2 || status != kcall.StatusOK || calls != 1 {
		t.Fatalf("WaitAny = %d, %v; handler calls=%d", mask, status, calls)
	}
}

func TestWaitHandlerBypassesPollingAndInvalidInputs(t *testing.T) {
	defer InstallWaitHandler(nil)
	calls := 0
	handler := WaitHandler(func(*[kcall.WaitAnyMaxObjects]Interest, int, uint64) (uint64, kcall.Status, bool) {
		calls++
		return 0, kcall.StatusOK, true
	})
	InstallWaitHandler(&handler)
	var interests [kcall.WaitAnyMaxObjects]Interest
	for _, count := range []int{-1, kcall.WaitAnyMaxObjects + 1} {
		if _, status := WaitAny(&interests, count, 17); status != kcall.StatusInvalidArgument {
			t.Fatalf("count %d: %v", count, status)
		}
	}
	if _, status := WaitAny(nil, 0, 17); status != kcall.StatusInvalidArgument {
		t.Fatalf("nil interests: %v", status)
	}
	WaitAny(&interests, 0, 0)
	if calls != 0 {
		t.Fatalf("polling or invalid input called handler %d times", calls)
	}
}

func TestWaitHandlerDeclinesToKernel(t *testing.T) {
	defer InstallWaitHandler(nil)
	var interests [kcall.WaitAnyMaxObjects]Interest
	InstallWaitHandler(nil)
	wantMask, wantStatus := WaitAny(&interests, 0, 1)
	handler := WaitHandler(func(*[kcall.WaitAnyMaxObjects]Interest, int, uint64) (uint64, kcall.Status, bool) {
		return 99, kcall.StatusOK, false
	})
	InstallWaitHandler(&handler)
	mask, status := WaitAny(&interests, 0, 1)
	if mask != wantMask || status != wantStatus {
		t.Fatalf("declined wait = %d, %v; native = %d, %v", mask, status, wantMask, wantStatus)
	}
}

func TestThreadJoinHandlerDispatchAndPolling(t *testing.T) {
	defer InstallThreadJoinHandler(nil)
	wantCode, wantStatus := ThreadJoin(9, true)
	calls := 0
	handler := ThreadJoinHandler(func(thread uint64) (uint64, kcall.Status, bool) {
		calls++
		if thread != 9 {
			t.Fatalf("join handler(%d)", thread)
		}
		// The adapter polls through the raw call without re-entering itself.
		if code, status := ThreadJoin(thread, true); code != wantCode || status != wantStatus {
			t.Fatalf("raw poll = %d, %v; want %d, %v", code, status, wantCode, wantStatus)
		}
		return 42, kcall.StatusOK, true
	})
	InstallThreadJoinHandler(&handler)
	if code, status := ThreadJoin(9, false); code != 42 || status != kcall.StatusOK || calls != 1 {
		t.Fatalf("join = %d, %v; calls=%d", code, status, calls)
	}
	if code, status := ThreadJoin(9, true); code != wantCode || status != wantStatus || calls != 1 {
		t.Fatalf("poll = %d, %v; calls=%d", code, status, calls)
	}
	InstallThreadJoinHandler(nil)
	wantCode, wantStatus = ThreadJoin(9, false)
	handler = func(uint64) (uint64, kcall.Status, bool) { return 99, kcall.StatusOK, false }
	InstallThreadJoinHandler(&handler)
	if code, status := ThreadJoin(9, false); code != wantCode || status != wantStatus {
		t.Fatalf("declined join = %d, %v; want native %d, %v", code, status, wantCode, wantStatus)
	}
}

func TestCursorWaitHandlerDispatchAndPolling(t *testing.T) {
	defer InstallCursorWaitHandler(nil)
	var interests [kcall.WaitAnyMaxObjects]CursorInterest
	interests[0] = CursorInterest{Handle: 7, Events: kcall.WaitReadable, Cursor: 99}
	wantMask, wantStatus := WaitAnyCursor(&interests, 1, 0)
	calls := 0
	handler := CursorWaitHandler(func(got *[kcall.WaitAnyMaxObjects]CursorInterest, count int, timeout uint64) (uint64, kcall.Status, bool) {
		calls++
		if got != &interests || count != 1 || timeout != 17 || got[0].Cursor != 99 {
			t.Fatalf("cursor handler arguments: %p %d %d %v", got, count, timeout, got[0])
		}
		if mask, status := WaitAnyCursor(got, count, 0); mask != wantMask || status != wantStatus {
			t.Fatalf("raw probe = %d, %v", mask, status)
		}
		return 1, kcall.StatusOK, true
	})
	InstallCursorWaitHandler(&handler)
	if mask, status := WaitAnyCursor(&interests, 1, 17); mask != 1 || status != kcall.StatusOK || calls != 1 {
		t.Fatalf("cursor wait = %d, %v; calls=%d", mask, status, calls)
	}
	WaitAnyCursor(&interests, 1, 0)
	for _, count := range []int{-1, kcall.WaitAnyMaxObjects + 1} {
		if _, status := WaitAnyCursor(&interests, count, 17); status != kcall.StatusInvalidArgument {
			t.Fatalf("count %d: %v", count, status)
		}
	}
	if _, status := WaitAnyCursor(nil, 0, 17); status != kcall.StatusInvalidArgument || calls != 1 {
		t.Fatalf("nil interests: %v, calls=%d", status, calls)
	}
	InstallCursorWaitHandler(nil)
	wantMask, wantStatus = WaitAnyCursor(&interests, 1, 17)
	handler = func(*[kcall.WaitAnyMaxObjects]CursorInterest, int, uint64) (uint64, kcall.Status, bool) {
		return 99, kcall.StatusOK, false
	}
	InstallCursorWaitHandler(&handler)
	if mask, status := WaitAnyCursor(&interests, 1, 17); mask != wantMask || status != wantStatus {
		t.Fatalf("declined cursor wait = %d, %v; native = %d, %v", mask, status, wantMask, wantStatus)
	}
}

func TestIRQWaitHandlerDispatch(t *testing.T) {
	defer InstallIRQWaitHandler(nil)
	InstallIRQWaitHandler(nil)
	want := IRQWait(5)
	calls := 0
	handler := IRQWaitHandler(func(irq handle.ID) (kcall.Status, bool) {
		calls++
		if irq != 5 {
			t.Fatalf("IRQ handler(%d)", irq)
		}
		return kcall.StatusOK, true
	})
	InstallIRQWaitHandler(&handler)
	if status := IRQWait(5); status != kcall.StatusOK || calls != 1 {
		t.Fatalf("IRQ wait = %v; calls=%d", status, calls)
	}
	if status := IRQPoll(5); status != want || calls != 1 {
		t.Fatalf("IRQ poll dispatched the handler: %v; calls=%d", status, calls)
	}
	handler = func(handle.ID) (kcall.Status, bool) { return kcall.StatusOK, false }
	InstallIRQWaitHandler(&handler)
	if status := IRQWait(5); status != want {
		t.Fatalf("declined IRQ wait = %v, want native %v", status, want)
	}
}
