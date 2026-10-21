//go:build goos_userspace

package main

import (
	"runtime"
	"sync/atomic"
	"time"

	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
)

// waitsets distinguishes a forbidden duplicate within one native wait from
// valid overlapping subscriptions belonging to separate goroutines, then
// joins a native thread from a parked goroutine.
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
	joinParked()
	ok("GOPROBE_WAITSETS_OK")
}

// joinGate holds the native thread joinParked joins until the joiner has
// parked.
var joinGate chan struct{}

func joinTarget(uintptr) { <-joinGate }

// joinParked joins a native thread from a goroutine while another goroutine
// keeps the only permit busy: the joiner parks on the thread's end instead
// of blocking its thread, a second goroutine joiner is refused as a second
// native joiner is, and the end wakes the first with the thread's code.
func joinParked() {
	runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(runtime.NumCPU())
	joinGate = make(chan struct{})
	var baseline goroutine.Stats
	goroutine.ReadStats(&baseline)
	id, status := thread.Start(joinTarget, 0)
	check(status == kcall.StatusOK, "start joined thread")
	// Exercise the nonblocking reservation ABI before the adapter owns it.
	token, status := sys.ThreadJoinClaim(uint64(id))
	check(status == kcall.StatusOK && token != 0, "claim a native join")
	_, status = sys.ThreadJoin(uint64(id), true)
	check(status == kcall.StatusAlreadyExists, "a native poll bypassed the token claim")
	check(sys.ThreadJoinRelease(uint64(id), token) == kcall.StatusOK, "release a native join claim")
	check(sys.ThreadJoinRelease(uint64(id), token) == kcall.StatusNotFound, "a released token still owned the join")
	type answer struct {
		code   uint64
		status kcall.Status
	}
	joined := make(chan answer, 1)
	go func() {
		code, status := thread.Join(id)
		joined <- answer{code, status}
	}()
	deadline := now() + 2e9
	for {
		var stats goroutine.Stats
		goroutine.ReadStats(&stats)
		if stats.Waiters == baseline.Waiters+1 {
			break
		}
		check(now() < deadline, "the joiner did not park on the thread's end")
		runtime.Gosched()
	}
	_, status = thread.Join(id)
	check(status == kcall.StatusAlreadyExists, "a second joiner was not refused")
	_, status = sys.ThreadJoin(uint64(id), true)
	check(status == kcall.StatusAlreadyExists, "a native poll bypassed the parked joiner")
	var spins atomic.Uint64
	stop := make(chan struct{})
	spun := make(chan struct{})
	go func() {
		defer close(spun)
		for {
			select {
			case <-stop:
				return
			default:
				spins.Add(1)
				runtime.Gosched()
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case result := <-joined:
		record("GOPROBE_FAILURE").field("code", result.code).field("status", uint64(result.status)).end()
		fail("the join returned before the thread ended")
	default:
	}
	close(joinGate)
	result := <-joined
	close(stop)
	<-spun
	check(result.status == kcall.StatusOK && result.code == 0, "the parked join did not collect the thread's code")
	_, status = thread.Join(id)
	check(status == kcall.StatusNotFound, "a joined thread was joined again")
	check(spins.Load() > 0, "no goroutine ran while the joiner waited")
	var after goroutine.Stats
	goroutine.ReadStats(&after)
	check(after.Waiters == baseline.Waiters, "the join left its wait registered")
	record("GOPROBE_WAITSETS_JOIN").field("spins", spins.Load()).end()
}
