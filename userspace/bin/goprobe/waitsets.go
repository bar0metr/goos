//go:build goos_userspace

package main

import (
	"runtime"
	"time"

	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/sys"
)

// waitsets distinguishes a forbidden duplicate within one native wait from
// valid overlapping subscriptions belonging to separate goroutines.
func waitsets() {
	runtime.GOMAXPROCS(1)
	first, firstWriter, status := sys.StreamCreate()
	check(status == kcall.StatusOK, "first wait-set stream")
	second, secondWriter, status := sys.StreamCreate()
	check(status == kcall.StatusOK, "second wait-set stream")
	alias, status := sys.HandleDuplicate(first, capability.Read|capability.Wait)
	check(status == kcall.StatusOK, "duplicate stream endpoint")
	defer sys.HandleClose(first)
	defer sys.HandleClose(firstWriter)
	defer sys.HandleClose(second)
	defer sys.HandleClose(secondWriter)
	defer sys.HandleClose(alias)
	interests := new([kcall.WaitAnyMaxObjects]sys.Interest)
	interests[0] = sys.Interest{Handle: first, Events: kcall.WaitReadable}
	for _, endpoint := range []handle.ID{first, alias} {
		interests[1] = sys.Interest{Handle: endpoint, Events: kcall.WaitPeerClosed}
		for _, timeout := range []uint64{0, 1, kcall.WaitAnyInfiniteTimeout} {
			mask, status := sys.WaitAny(interests, 2, timeout)
			check(status == kcall.StatusResourceExhausted && mask == 0, "duplicate wait set did not fail before waiting")
		}
	}
	var baseline goroutine.Stats
	goroutine.ReadStats(&baseline)
	type answer struct {
		mask   uint64
		status kcall.Status
	}
	both, one := make(chan answer, 1), make(chan answer, 1)
	go func() {
		set := new([kcall.WaitAnyMaxObjects]sys.Interest)
		set[0] = sys.Interest{Handle: first, Events: kcall.WaitReadable}
		set[1] = sys.Interest{Handle: second, Events: kcall.WaitReadable}
		mask, status := sys.WaitAny(set, 2, 100)
		both <- answer{mask, status}
	}()
	go func() {
		set := new([kcall.WaitAnyMaxObjects]sys.Interest)
		set[0] = sys.Interest{Handle: alias, Events: kcall.WaitReadable}
		mask, status := sys.WaitAny(set, 1, 100)
		one <- answer{mask, status}
	}()
	deadline := now() + 2e9
	for {
		var stats goroutine.Stats
		goroutine.ReadStats(&stats)
		if stats.Waiters == baseline.Waiters+3 {
			break
		}
		check(now() < deadline, "overlapping waits did not park")
		runtime.Gosched()
	}
	// Let the idle poller encounter the aliased aggregate before either
	// endpoint becomes ready. It must sleep for a bounded fallback tick.
	time.Sleep(20 * time.Millisecond)
	for _, writer := range []handle.ID{firstWriter, secondWriter} {
		count, status := sys.StreamWrite(writer, []byte{1})
		check(status == kcall.StatusOK && count == 1, "make wait-set endpoint ready")
	}
	a, b := <-both, <-one
	check(a.status == kcall.StatusOK && a.mask&1 != 0 && a.mask&^uint64(3) == 0, "group probe returned invalid readiness")
	check(b.status == kcall.StatusOK && b.mask == 1, "aliased subscriber did not wake")
	interests[1] = sys.Interest{Handle: second, Events: kcall.WaitReadable}
	mask, status := sys.WaitAny(interests, 2, 0)
	check(status == kcall.StatusOK && mask == 3, "two ready interests lost bits")
	for _, endpoint := range []handle.ID{first, alias} {
		interests[1] = sys.Interest{Handle: endpoint, Events: kcall.WaitReadable}
		for _, timeout := range []uint64{0, 1, kcall.WaitAnyInfiniteTimeout} {
			mask, status := sys.WaitAny(interests, 2, timeout)
			check(status == kcall.StatusResourceExhausted && mask == 0, "ready duplicate set bypassed validation")
		}
	}
	var after goroutine.Stats
	goroutine.ReadStats(&after)
	check(after.Waiters == baseline.Waiters, "wait-set subscriptions leaked")
	record("GOPROBE_WAITSETS").field("polls", after.Polls-baseline.Polls).field("mask", a.mask).end()
	ok("GOPROBE_WAITSETS_OK")
}
