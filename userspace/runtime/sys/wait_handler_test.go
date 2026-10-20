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
