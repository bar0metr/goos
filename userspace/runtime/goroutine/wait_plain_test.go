package goroutine

import (
	"testing"

	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

func TestPlainWaitKeepsOriginalKernelMode(t *testing.T) {
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: 7, Events: kcall.WaitReadable}
	// In particular, a log handle in a plain interest must keep the native
	// Unsupported result, rather than acquiring cursor zero in the poller.
	for _, want := range []kcall.Status{kcall.StatusUnsupported, kcall.StatusResourceExhausted, kcall.StatusPermissionDenied, kcall.StatusOK} {
		ops := plainWaitOps{
			probe: func(got *[kcall.WaitAnyMaxObjects]sys.Interest, count int, timeout uint64) (uint64, kcall.Status) {
				if got != &interests || count != 1 || timeout != 0 {
					t.Fatal("plain validation changed the original set")
				}
				return 1, want
			},
			// Neither an invalid set nor immediate readiness may register or
			// reach aggregate alias fallback.
			park: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64, *Cancel) (uint64, kcall.Status) {
				t.Fatal("a non-timeout native result reached cursor registration")
				return 0, kcall.StatusOK
			},
		}
		if mask, status := waitPlain(&interests, 1, 100, nil, ops); mask != 1 || status != want {
			t.Fatalf("plain result=%d,%v want=1,%v", mask, status, want)
		}
	}
}

func TestPlainWaitRegistersOnlyValidatedInterests(t *testing.T) {
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	var cancel Cancel
	probes, parks := 0, 0
	ops := plainWaitOps{
		probe: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, uint64) (uint64, kcall.Status) {
			probes++
			return 0, kcall.StatusTimeout
		},
		park: func(got *[kcall.WaitAnyMaxObjects]sys.Interest, count int, deadline int64, c *Cancel) (uint64, kcall.Status) {
			parks++
			if got != &interests || count != 1 || deadline != 100 || c != &cancel {
				t.Fatal("validated park changed its arguments")
			}
			return 1, kcall.StatusOK
		},
	}
	if mask, status := waitPlain(&interests, 1, 100, &cancel, ops); mask != 1 || status != kcall.StatusOK || probes != 1 || parks != 1 {
		t.Fatalf("result=%d,%v probes=%d parks=%d", mask, status, probes, parks)
	}
	for _, count := range []int{-1, MaxWaiters + 1} {
		if _, status := waitPlain(&interests, count, 0, nil, plainWaitOps{}); status != kcall.StatusInvalidArgument {
			t.Fatalf("count %d result=%v", count, status)
		}
	}
	if _, status := waitPlain(nil, 0, 0, nil, plainWaitOps{}); status != kcall.StatusInvalidArgument {
		t.Fatalf("nil interests=%v", status)
	}
}
