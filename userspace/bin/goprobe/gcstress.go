//go:build goos_userspace

package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/memory"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
)

// Collector stress (ADR 0204). "gcstress N ROUNDS" runs one scenario per
// root class the goroutine runtime adds. In each, heap lists are reachable
// only through that class while N goroutines force collections, and every
// list is checked afterwards. Validation images poison what a collection
// frees (arena poison_on.go), so a list freed while live fails its check
// at the next read instead of when its slot is reused. ROUNDS scales each
// scenario's length; each round lasts stressRound.
//
//	preempt     goroutines spin on a list held only in registers and yield
//	            holding one in callee-saved registers
//	create      lists passed only as go statement arguments and returned
//	            through channels while goroutines start and end
//	defer       lists captured only by pending defers and carried by panics,
//	            with collections inside the deferred calls
//	chan        lists only in channel buffers and in blocked senders
//	timers      lists only in pending time.AfterFunc closures
//	switch      lists on goroutine stacks across sleeps, handle waits, and
//	            yields, which run on the scheduler stack
//	threads     thread.Start threads exchanging lists with goroutines
//	rendezvous  a thread held inside a region transition makes collections
//	            give up while goroutines are parked; the next one runs
//
// The lists the scenarios drop are sampled through tokens, which do not
// keep them alive; once the scenarios end, most samples must read changed,
// so a collector that frees nothing fails.
const (
	stressLength = 32
	stressMagic  = 0x9e3779b97f4a7c15
	stressRound  = 40 * time.Millisecond
	// stressSamples bounds the dropped lists sampled for reclamation.
	stressSamples = 256
	// stressTimeout bounds every wait for another goroutine or thread.
	stressTimeout = 30 * time.Second
)

// cell is a checked list cell: 48 bytes, so every list shares one class.
type cell struct {
	next  *cell
	value uint64
	check uint64
	pad   [3]uint64
}

//go:noinline
func buildCells(seed uint64) *cell {
	var head *cell
	for i := stressLength - 1; i >= 0; i-- {
		v := seed + uint64(i)
		head = &cell{next: head, value: v, check: v ^ stressMagic}
	}
	return head
}

// cellsIntact reports whether the list at head holds the cells buildCells
// made from seed. It reads each cell's check before its next pointer, so a
// poisoned cell is reported, not followed.
//
//go:noinline
func cellsIntact(head *cell, seed uint64) bool {
	n := head
	for i := 0; i < stressLength; i++ {
		v := seed + uint64(i)
		if n == nil || n.value != v || n.check != v^stressMagic {
			return false
		}
		n = n.next
	}
	return n == nil
}

// stressItem carries a list and the seed it was built from.
type stressItem struct {
	head *cell
	seed uint64
}

// stressRun is one gcstress run's shared state.
type stressRun struct {
	collectors int
	rounds     int
	seed       atomic.Uint64
	lost       atomic.Uint32
	checked    atomic.Uint64
	// samples holds tokens of dropped lists' first cells and their values.
	samples   [stressSamples]struct{ token, value uint64 }
	sampled   atomic.Uint32
	collected atomic.Uint64
}

func (r *stressRun) nextSeed() uint64 { return r.seed.Add(1 << 16) }

// verify checks a list and counts a lost one.
func (r *stressRun) verify(head *cell, seed uint64) {
	r.checked.Add(1)
	if !cellsIntact(head, seed) {
		r.lost.Add(1)
	}
}

// sample records a list that is about to be dropped.
//
//go:noinline
func (r *stressRun) sample(head *cell, seed uint64) {
	index := r.sampled.Add(1) - 1
	if index < stressSamples {
		r.samples[index].token, r.samples[index].value = arena.Token(head), seed
	}
}

// pressure runs collectors goroutines that churn and collect until stop.
type pressure struct {
	stop atomic.Bool
	wg   sync.WaitGroup
}

func (r *stressRun) startPressure() *pressure {
	p := &pressure{}
	for i := 0; i < r.collectors; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for !p.stop.Load() {
				churn(4)
				runtime.GC()
				r.collected.Add(1)
				runtime.Gosched()
			}
		}()
	}
	return p
}

func (p *pressure) end() {
	p.stop.Store(true)
	p.wg.Wait()
}

// duration is how long a scenario's main phase runs.
func (r *stressRun) duration() time.Duration { return time.Duration(r.rounds) * stressRound }

// scenario reports one scenario and fails the run when it lost a list.
func (r *stressRun) scenario(name string, before arena.Stats, extra string, value uint64) {
	after := arena.HeapStats()
	l := record("GOPROBE_GCSTRESS_SCENARIO").text(" name=").text(name).
		field("checked", r.checked.Load()).field("lost", uint64(r.lost.Load())).
		field("collections", uint64(after.Collections-before.Collections)).
		field("live", after.Live).field("objects", after.Objects).
		field("mallocs", after.Mallocs).field("frees", after.Frees)
	if extra != "" {
		l.field(extra, value)
	}
	l.end()
	check(r.lost.Load() == 0, "gcstress: a live list was freed")
	check(after.Collections > before.Collections, "gcstress: no collection ran during a scenario")
	r.checked.Store(0)
}

// gcStress runs every scenario with collectors goroutines forcing
// collections.
//
//go:noinline
func gcStress(collectors, rounds, taskLimit int) {
	if collectors < 1 {
		collectors = 1
	}
	if rounds < 1 {
		rounds = 8
	}
	check(collectors <= 8 && rounds <= 64, "gcstress: use N <= 8 and ROUNDS <= 64")
	// This process keeps its scheduler workers until exit. Reserve native task
	// slots before the first scenario creates them, so the later thread.Start
	// workers can coexist with goroutines on the large-profile task quota.
	check(taskLimit > len(stressLinks), "gcstress: task quota cannot hold native threads")
	if maximum := taskLimit - len(stressLinks); runtime.GOMAXPROCS(0) > maximum {
		runtime.GOMAXPROCS(maximum)
	}
	r := &stressRun{collectors: collectors, rounds: rounds}
	baseline := arena.HeapStats()
	poisoned := arena.PoisonedBytes()
	swap0, preempt0 := arena.SwitchHolds()
	var g0 goroutine.Stats
	goroutine.ReadStats(&g0)

	step("gcstress preempt")
	stressPreempt(r)
	step("gcstress create")
	stressCreate(r)
	step("gcstress defer")
	stressDefer(r)
	step("gcstress chan")
	stressChannels(r)
	step("gcstress timers")
	stressTimers(r)
	step("gcstress switch")
	stressSwitch(r)
	step("gcstress threads")
	stressThreads(r)
	step("gcstress rendezvous")
	stressRendezvous(r)

	// WaitGroup completion precedes scheduler retirement on another M.
	// Retire workers before measuring their stacks and dropped heap roots.
	retireDeadline := time.Now().Add(stressTimeout)
	for {
		var stats goroutine.Stats
		goroutine.ReadStats(&stats)
		if stats.Live == 1 {
			break
		}
		check(time.Now().Before(retireDeadline), "gcstress: goroutines did not retire")
		time.Sleep(time.Millisecond)
	}

	// Every list is garbage now. Most sampled ones must read changed:
	// freed and poisoned, or freed and reused. Conservative scanning may
	// keep a few through stale words.
	beforeFinal := arena.HeapStats().Collections
	finalDeadline := time.Now().Add(stressTimeout)
	var after arena.Stats
	for {
		runtime.GC()
		after = arena.HeapStats()
		if after.Collections-beforeFinal >= 3 {
			break
		}
		check(time.Now().Before(finalDeadline), "gcstress: final collections did not complete")
		runtime.Gosched()
	}
	samples := int(min(r.sampled.Load(), stressSamples))
	reclaimed := 0
	for index := 0; index < samples; index++ {
		if peekValue(r.samples[index].token) != r.samples[index].value {
			reclaimed++
		}
	}
	var g1 goroutine.Stats
	goroutine.ReadStats(&g1)
	swap1, preempt1 := arena.SwitchHolds()
	record("GOPROBE_GCSTRESS").field("collectors", uint64(collectors)).field("rounds", uint64(rounds)).
		field("collections", uint64(after.Collections-baseline.Collections)).field("requests", r.collected.Load()).
		field("final_collections", uint64(after.Collections-beforeFinal)).
		field("sampled", uint64(samples)).field("reclaimed", uint64(reclaimed)).
		field("poisoned", arena.PoisonedBytes()-poisoned).
		field("preemptions", g1.Preemptions-g0.Preemptions).field("created", g1.Created-g0.Created).
		field("swap_holds", swap1-swap0).field("preempt_holds", preempt1-preempt0).
		field("live_before", baseline.Live).field("live_after", after.Live).
		field("objects", after.Objects).field("mallocs", after.Mallocs).
		field("frees", after.Frees).end()
	check(samples >= 64, "gcstress: too few dropped lists sampled")
	check(2*reclaimed >= samples, "gcstress: dropped lists were not reclaimed")
	check(arena.PoisonedBytes() > poisoned, "gcstress: sweep poisoned nothing in a validation image")
	check(g1.Live == 1, "gcstress: goroutines outlived the run")
	check(after.Live < baseline.Live+512<<10, "gcstress: garbage was not reclaimed")
	ok("GOPROBE_GCSTRESS_OK")
}

// peekValue reads the value word of the cell a token names, freed or not.
// The heap reservation stays mapped, and a page given back reads zero.
//
//go:noinline
func peekValue(token uint64) uint64 {
	return memory.Word(uintptr(^token) + cellValueOffset)
}

// cellValueOffset is where cell.value lies: after the next pointer.
const cellValueOffset = 8

// stressPreempt runs more spinning goroutines than permits, so preemption
// switches them while their list is only in registers, and goroutines that
// yield holding a list in callee-saved registers, which goos_g_swap saves.
func stressPreempt(r *stressRun) {
	before := arena.HeapStats()
	var g0 goroutine.Stats
	goroutine.ReadStats(&g0)
	p := r.startPressure()
	spinners := 2*runtime.GOMAXPROCS(0) + 2
	deadline := time.Now().Add(r.duration())
	// Collections hold the threads for much of the run, and a thread is
	// visited only after PreemptTicks of its own user time: the spinners
	// run on until one was preempted, up to stressTimeout.
	limit := time.Now().Add(stressTimeout)
	preempted := func() bool {
		var s goroutine.Stats
		goroutine.ReadStats(&s)
		return s.Preemptions > g0.Preemptions
	}
	var wg sync.WaitGroup
	var mixed atomic.Uint64
	for i := 0; i < spinners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for walks := 0; ; walks++ {
				if walks >= 2 && !time.Now().Before(deadline) && (preempted() || !time.Now().Before(limit)) {
					break
				}
				seed := r.nextSeed()
				var ok bool
				var mix uint64
				if i%3 == 2 {
					ok = yieldHolding(buildCells(seed), seed, 8)
				} else {
					ok, mix = walkSpin(buildCells(seed), seed, 2000)
				}
				r.checked.Add(1)
				if !ok {
					r.lost.Add(1)
				}
				mixed.Add(mix)
			}
		}(i)
	}
	wg.Wait()
	p.end()
	var g1 goroutine.Stats
	goroutine.ReadStats(&g1)
	r.scenario("preempt", before, "preemptions", g1.Preemptions-g0.Preemptions)
	check(g1.Preemptions > g0.Preemptions, "gcstress: no spinning goroutine was preempted")
}

// walkSpin walks a list it alone holds, spinning on each cell without a
// call, so the cell pointer stays in a register; the cells behind it become
// garbage as it goes.
//
//go:noinline
func walkSpin(n *cell, seed uint64, work int) (bool, uint64) {
	mix := uint64(0)
	for i := 0; i < stressLength; i++ {
		v := seed + uint64(i)
		if n == nil || n.value != v || n.check != v^stressMagic {
			return false, mix
		}
		for k := 0; k < work; k++ {
			mix = mix*6364136223846793005 + n.value + uint64(k)
		}
		n = n.next
	}
	return n == nil, mix
}

// yieldHolding yields with the list live across each call.
//
//go:noinline
func yieldHolding(head *cell, seed uint64, yields int) bool {
	for k := 0; k < yields; k++ {
		runtime.Gosched()
	}
	return cellsIntact(head, seed)
}

// stressCreate starts goroutines whose only reference to a list is their
// go statement argument, from several creators at once, and takes their
// result lists back through a channel.
func stressCreate(r *stressRun) {
	before := arena.HeapStats()
	p := r.startPressure()
	creators := max(2, runtime.GOMAXPROCS(0))
	deadline := time.Now().Add(r.duration())
	var wg sync.WaitGroup
	var children atomic.Uint64
	for c := 0; c < creators; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			const batch = 8
			results := make(chan stressItem, batch)
			for rounds := 0; rounds < 2 || time.Now().Before(deadline); rounds++ {
				for k := 0; k < batch; k++ {
					seed := r.nextSeed()
					go stressChild(r, buildCells(seed), seed, results)
				}
				for k := 0; k < batch; k++ {
					item := <-results
					r.verify(item.head, item.seed)
				}
				children.Add(batch)
			}
		}()
	}
	wg.Wait()
	p.end()
	r.scenario("create", before, "children", children.Load())
}

// stressChild checks the list it was started with, drops it, and returns a
// new one through results.
//
//go:noinline
func stressChild(r *stressRun, head *cell, seed uint64, results chan<- stressItem) {
	r.verify(head, seed)
	r.sample(head, seed)
	reply := r.nextSeed()
	results <- stressItem{buildCells(reply), reply}
}

// stressDefer runs goroutines whose lists are held by pending deferred
// closures or carried by a panic, collecting inside the deferred calls, on
// normal return, on panic and recover, and on runtime.Goexit.
func stressDefer(r *stressRun) {
	before := arena.HeapStats()
	p := r.startPressure()
	deadline := time.Now().Add(r.duration())
	var wg sync.WaitGroup
	var exits atomic.Uint64
	for w := 0; w < max(2, runtime.GOMAXPROCS(0)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rounds := 0; rounds < 2 || time.Now().Before(deadline); rounds++ {
				var done sync.WaitGroup
				done.Add(3)
				go deferReturn(r, r.nextSeed(), &done)
				go deferPanic(r, r.nextSeed(), &done)
				go deferGoexit(r, r.nextSeed(), &done)
				done.Wait()
				exits.Add(3)
			}
		}()
	}
	wg.Wait()
	p.end()
	r.scenario("defer", before, "exits", exits.Load())
}

//go:noinline
func deferReturn(r *stressRun, seed uint64, done *sync.WaitGroup) {
	defer done.Done()
	a := buildCells(seed)
	defer func() {
		runtime.GC()
		r.verify(a, seed)
	}()
	b := buildCells(seed + 1000)
	defer func() {
		churn(2)
		runtime.GC()
		r.verify(b, seed+1000)
	}()
}

//go:noinline
func deferPanic(r *stressRun, seed uint64, done *sync.WaitGroup) {
	defer done.Done()
	a := buildCells(seed)
	defer func() {
		value := recover()
		runtime.GC()
		carried, ok := value.(*cell)
		r.checked.Add(1)
		if !ok {
			r.lost.Add(1)
		} else {
			r.verify(carried, seed+2000)
		}
		r.verify(a, seed)
	}()
	defer func() {
		// Runs while the panic unwinds, before the recover.
		runtime.GC()
		r.verify(a, seed)
	}()
	panic(buildCells(seed + 2000))
}

//go:noinline
func deferGoexit(r *stressRun, seed uint64, done *sync.WaitGroup) {
	defer done.Done()
	a := buildCells(seed)
	defer func() {
		runtime.GC()
		r.verify(a, seed)
	}()
	runtime.Goexit()
}

// stressChannels keeps lists only in a buffered channel's slots, in
// senders blocked on an unbuffered channel, on a full buffered channel, and
// in a select, while collections run.
func stressChannels(r *stressRun) {
	before := arena.HeapStats()
	p := r.startPressure()
	const buffered, senders = 16, 8
	for round := 0; round < r.rounds; round++ {
		full := make(chan stressItem, buffered)
		unbuffered := make(chan stressItem)
		selected := make(chan stressItem)
		never := make(chan struct{})
		for k := 0; k < buffered; k++ {
			fillChannel(r, full)
		}
		var wg sync.WaitGroup
		for k := 0; k < senders; k++ {
			wg.Add(3)
			go func() {
				defer wg.Done()
				seed := r.nextSeed()
				unbuffered <- stressItem{buildCells(seed), seed}
			}()
			go func() {
				defer wg.Done()
				seed := r.nextSeed()
				full <- stressItem{buildCells(seed), seed}
			}()
			go func() {
				defer wg.Done()
				seed := r.nextSeed()
				select {
				case selected <- stressItem{buildCells(seed), seed}:
				case <-never:
				}
			}()
		}
		awaitParked(3 * senders)
		runtime.GC()
		time.Sleep(stressRound)
		for k := 0; k < senders; k++ {
			item := <-unbuffered
			r.verify(item.head, item.seed)
			item = <-selected
			r.verify(item.head, item.seed)
		}
		for k := 0; k < buffered+senders; k++ {
			item := <-full
			r.verify(item.head, item.seed)
			if k%4 == 0 {
				r.sample(item.head, item.seed)
			}
		}
		wg.Wait()
	}
	p.end()
	r.scenario("chan", before, "", 0)
}

//go:noinline
func fillChannel(r *stressRun, c chan<- stressItem) {
	seed := r.nextSeed()
	c <- stressItem{buildCells(seed), seed}
}

// stressTimers keeps lists only in pending time.AfterFunc closures, due
// over several rounds, and in stopped timers' closures, which must become
// garbage.
func stressTimers(r *stressRun) {
	before := arena.HeapStats()
	p := r.startPressure()
	const count = 32
	results := make(chan bool, count)
	for k := 0; k < count; k++ {
		armTimer(r, time.Duration(1+k%r.rounds)*stressRound, results)
	}
	var stopped [count / 2]*time.Timer
	for k := range stopped {
		stopped[k] = armStopped(r)
	}
	stoppedCount := 0
	for k := range stopped {
		if stopped[k].Stop() {
			stoppedCount++
		}
		stopped[k] = nil
	}
	timeout := time.After(stressTimeout)
	for k := 0; k < count; k++ {
		select {
		case intact := <-results:
			r.checked.Add(1)
			if !intact {
				r.lost.Add(1)
			}
		case <-timeout:
			fail("gcstress: a timer callback did not run")
		}
	}
	p.end()
	r.scenario("timers", before, "stopped", uint64(stoppedCount))
	check(stoppedCount == count/2, "gcstress: a far timer fired before Stop")
}

//go:noinline
func armTimer(r *stressRun, after time.Duration, results chan<- bool) {
	seed := r.nextSeed()
	head := buildCells(seed)
	time.AfterFunc(after, func() {
		runtime.GC()
		intact := cellsIntact(head, seed)
		r.sample(head, seed)
		results <- intact
	})
}

//go:noinline
func armStopped(r *stressRun) *time.Timer {
	seed := r.nextSeed()
	head := buildCells(seed)
	r.sample(head, seed)
	return time.AfterFunc(time.Hour, func() { r.verify(head, seed) })
}

// stressSwitch keeps lists on goroutine stacks across calls that switch to
// the scheduler stack and back: sleeps, handle waits that time out, yields,
// and system calls.
func stressSwitch(r *stressRun) {
	before := arena.HeapStats()
	p := r.startPressure()
	deadline := time.Now().Add(r.duration())
	var wg sync.WaitGroup
	var switches atomic.Uint64
	for w := 0; w < 2*runtime.GOMAXPROCS(0)+2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			receiver, sender, status := sys.StreamCreate()
			check(status == kcall.StatusOK, "gcstress: switch stream")
			for rounds := 0; rounds < 4 || time.Now().Before(deadline); rounds++ {
				seed := r.nextSeed()
				switchHolding(r, buildCells(seed), seed, w+rounds, receiver)
				switches.Add(1)
			}
			sys.HandleClose(sender)
			sys.HandleClose(receiver)
		}(w)
	}
	wg.Wait()
	p.end()
	r.scenario("switch", before, "switches", switches.Load())
}

// switchHolding holds head across one switching call and checks it.
//
//go:noinline
func switchHolding(r *stressRun, head *cell, seed uint64, kind int, receiver handle.ID) {
	switch kind % 4 {
	case 0:
		time.Sleep(time.Millisecond)
	case 1:
		receive(receiver, now()+1e6, nil)
	case 2:
		runtime.Gosched()
	case 3:
		sys.ThreadYield()
	}
	r.verify(head, seed)
}

// stressLinks are the channels the threads scenario's threads serve.
var stressLinks [2]struct{ in, out chan stressItem }

// stressThreads runs thread.Start threads that take lists from goroutines,
// hold them across their own collections and yields, and answer with new
// lists.
func stressThreads(r *stressRun) {
	before := arena.HeapStats()
	p := r.startPressure()
	var ids [len(stressLinks)]thread.ID
	for index := range stressLinks {
		stressLinks[index].in = make(chan stressItem)
		stressLinks[index].out = make(chan stressItem)
		id, status := thread.Start(stressThread, uintptr(index))
		if status != kcall.StatusOK {
			record("GOPROBE_GCSTRESS_THREAD_START_FAILED").field("status", uint64(status)).field("index", uint64(index)).end()
		}
		check(status == kcall.StatusOK, "gcstress: thread start")
		ids[index] = id
	}
	var wg sync.WaitGroup
	exchanges := 4 * r.rounds
	for index := range stressLinks {
		for k := 0; k < 2; k++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				link := &stressLinks[index]
				for e := 0; e < exchanges; e++ {
					seed := r.nextSeed()
					link.in <- stressItem{buildCells(seed), seed}
					reply := <-link.out
					r.verify(reply.head, reply.seed)
				}
			}(index)
		}
	}
	wg.Wait()
	for index := range stressLinks {
		close(stressLinks[index].in)
		code, status := thread.Join(ids[index])
		check(status == kcall.StatusOK && code == 0, "gcstress: thread join")
		stressLinks[index].in, stressLinks[index].out = nil, nil
	}
	p.end()
	r.scenario("threads", before, "exchanges", uint64(2*len(stressLinks)*exchanges))
}

// stressThread answers each list it receives with a new one, or with none
// when the list it held was lost.
func stressThread(index uintptr) {
	link := &stressLinks[index]
	for item := range link.in {
		intact := cellsIntact(item.head, item.seed)
		runtime.GC()
		sys.ThreadYield()
		intact = intact && cellsIntact(item.head, item.seed)
		reply := stressItem{}
		if intact {
			reply.seed = item.seed + 1000
			reply.head = buildCells(reply.seed)
		}
		link.out <- reply
	}
}

// stressRendezvous parks goroutines holding lists, then holds a thread
// inside a region transition (arena region_hold.go), so every collection
// requested gives up without marking or sweeping. Once the thread is let
// go, a collection must run and every parked goroutine's list survive it.
func stressRendezvous(r *stressRun) {
	before := arena.HeapStats()
	gate := make(chan struct{})
	pending := make(chan stressItem)
	parked := 4 * runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	for k := 0; k < parked; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			seed := r.nextSeed()
			head := buildCells(seed)
			switch k % 3 {
			case 0:
				<-gate
			case 1:
				select {
				case <-gate:
				case <-time.After(time.Hour):
					fail("gcstress: parked select timed out")
				}
			case 2:
				pending <- stressItem{head, seed}
				return
			}
			r.verify(head, seed)
		}(k)
	}
	awaitParked(parked)
	var d0, d1 arena.Diagnostics
	arena.ReadDiagnostics(&d0)
	id, status := thread.Start(rendezvousMover, 0)
	check(status == kcall.StatusOK, "gcstress: mover start")
	deadline := time.Now().Add(stressTimeout)
	for !arena.RegionTransitionHeld() {
		check(time.Now().Before(deadline), "gcstress: the mover did not stop in its region transition")
		time.Sleep(time.Millisecond)
	}
	held := arena.HeapStats()
	const requests = 4
	for k := 0; k < requests; k++ {
		churn(4)
		runtime.GC()
	}
	other := make(chan struct{})
	go func() {
		runtime.GC()
		close(other)
	}()
	<-other
	arena.ReadDiagnostics(&d1)
	rejected := d1.SuspendFailures - d0.SuspendFailures
	check(arena.HeapStats().Collections == held.Collections, "gcstress: a collection ran while a thread was inside a region transition")
	check(rejected >= requests+1, "gcstress: a collection request did not give up")
	check(d1.RegionWindowRetries > d0.RegionWindowRetries, "gcstress: no region-window rejection")
	check(arena.ReleaseRegionTransition(), "gcstress: the mover was not held")
	code, status := thread.Join(id)
	check(status == kcall.StatusOK && code == 0, "gcstress: mover join")
	runtime.GC()
	check(arena.HeapStats().Collections > held.Collections, "gcstress: no collection after the window closed")
	close(gate)
	for k := 2; k < parked; k += 3 {
		item := <-pending
		r.verify(item.head, item.seed)
	}
	wg.Wait()
	r.scenario("rendezvous", before, "rejected", rejected)
}

// awaitParked waits for count blocked goroutines. Pressure workers do not
// park, so the channel and rendezvous fixtures reach their wait states
// before the collector runs. The caller yields rather than adding a timer.
func awaitParked(count int) {
	deadline := now() + int64(stressTimeout)
	for {
		var s goroutine.Stats
		goroutine.ReadStats(&s)
		if s.Parked >= uint64(count) {
			return
		}
		check(now() < deadline, "gcstress: goroutines did not park")
		runtime.Gosched()
	}
}

// rendezvousMover enters a region with the validation hold set, so it stops
// inside the transition until ReleaseRegionTransition.
func rendezvousMover(uintptr) {
	region := arena.NewRegion(64 << 10)
	arena.HoldRegionTransition()
	scope := region.Enter()
	scope.Leave()
	region.Release()
}
