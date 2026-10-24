//go:build goos_userspace

// Command goprobe checks goroutines from inside the running system (ADR
// 0204). It is built with the goroutine runtime and only into validation
// images. Each mode prints GOPROBE_<MODE> lines with its measurements and
// ends with GOPROBE_<MODE>_OK, or prints GOPROBE_FAIL and exits 1:
//
//	pool        the canonical bounded worker pool over channels
//	smoke N     N goroutines computing at once on N threads
//	chan        channels and select, closing, nil channels, panics
//	sync        Mutex, RWMutex, WaitGroup, Once, Cond, atomics
//	timers      time.Sleep, timers, tickers, and context deadlines
//	fair        a goroutine in a loop without calls cannot starve another,
//	            and preemption keeps its SSE registers
//	panic       panics, defers, and recover inside goroutines
//	limit       the goroutine limit stops a go statement in its creator
//	gc N        roots of parked, sleeping, waiting, and migrating goroutines
//	gcstress N ROUNDS  roots of every runtime root class under N collecting
//	            goroutines (gcstress.go)
//	regroots    a list held only in one register, for each XMM and
//	            general-purpose register, of a thread and of a goroutine
//	            (regroots.go)
//	io          handle waits that park only the goroutine
//	waitsets    duplicate-interest validation, overlapping waits, and a parked join
//	logwait     goroutines parked on a kernel log cursor until a record arrives
//	threads     thread.Start threads and goroutines together
//	bench       scheduler, channel, timer, and CPU scaling measurements
//	overflow    a goroutine that overflows its stack ends the process
//	deadlock    every goroutine asleep ends the process with code 2
package main

import (
	"context"
	"encoding/binary"
	"runtime"
	"runtime/volatile"
	"sync"
	"sync/atomic"
	"time"

	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/thread"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "goprobe mode [N [ROUNDS]]",
	Summary: "goroutine checks",
	Options: "modes: pool, smoke, chan, sync, timers, fair, panic, panicreport, limit, gc, gcstress, regroots, io, waitsets, logwait, threads, bench, overflow, deadlock",
	MaxArgs: 3,
}

//export userspaceMain
func userspaceMain() {
	goroutine.Init()
	utility.Run(manifest, run)
}

func main() {}

//go:noinline
func run(data startup.Data) {
	if data.ArgCount() < 1 {
		utility.Usage()
	}
	var processInfo [kcall.ProcessInfoSize]byte
	check(sys.ProcessInfo(kcall.ProcessInfoSelf, &processInfo) == kcall.StatusOK, "read own task quota")
	offset := kcall.ProcessInfoLimitsOffset + 4*kcall.ProcessTasks
	taskLimit := binary.LittleEndian.Uint32(processInfo[offset : offset+4])
	check(runtime.GOMAXPROCS(0) <= int(taskLimit) && runtime.GOMAXPROCS(0) <= runtime.NumCPU(), "default parallelism exceeds process quota or CPUs")
	mode, _ := data.Arg(1)
	argument := uint64(0)
	if data.ArgCount() >= 2 {
		value, _ := data.Arg(2)
		argument = parse(value)
	}
	rounds := uint64(0)
	if data.ArgCount() >= 3 {
		value, _ := data.Arg(3)
		rounds = parse(value)
	}
	switch {
	case utility.Equal(mode, "pool"):
		pool()
	case utility.Equal(mode, "smoke"):
		smoke(int(argument))
	case utility.Equal(mode, "chan"):
		channels()
	case utility.Equal(mode, "sync"):
		syncs()
	case utility.Equal(mode, "timers"):
		timers()
	case utility.Equal(mode, "fair"):
		fair()
	case utility.Equal(mode, "panic"):
		panics()
	case utility.Equal(mode, "panicreport"):
		panic("GOPROBE_PANIC_REPORT")
	case utility.Equal(mode, "limit"):
		limit()
	case utility.Equal(mode, "gc"):
		roots(int(argument))
	case utility.Equal(mode, "gcstress"):
		gcStress(int(argument), int(rounds), int(taskLimit))
	case utility.Equal(mode, "regroots"):
		regRoots(int(taskLimit))
	case utility.Equal(mode, "io"):
		waits()
	case utility.Equal(mode, "waitsets"):
		waitsets()
	case utility.Equal(mode, "logwait"):
		logwait(data)
	case utility.Equal(mode, "threads"):
		threads()
	case utility.Equal(mode, "bench"):
		bench()
	case utility.Equal(mode, "overflow"):
		overflow()
	case utility.Equal(mode, "deadlock"):
		deadlock()
	default:
		utility.Usage()
	}
	printStats()
}

func parse(value []byte) uint64 {
	result := uint64(0)
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			utility.Usage()
		}
		result = result*10 + uint64(digit-'0')
	}
	return result
}

// line builds one output record, written at once.
type line struct {
	buffer [512]byte
	length int
}

func (l *line) text(value string) *line {
	l.length += copy(l.buffer[l.length:], value)
	return l
}

func (l *line) field(name string, value uint64) *line {
	l.text(" ").text(name).text("=")
	l.length = len(utility.AppendUint(l.buffer[:l.length], value))
	return l
}

func (l *line) end() {
	l.text("\n")
	utility.Write(l.buffer[:l.length])
}

func record(name string) *line {
	l := &line{}
	l.text(name)
	return l
}

func ok(name string) { record(name).end() }

// step marks progress inside a mode, so a failure report says how far it
// got.
func step(name string) { record("GOPROBE_STEP ").text(name).end() }

func fail(reason string) {
	record("GOPROBE_FAIL ").text(reason).end()
	printStats()
	utility.Exit(1)
}

func check(condition bool, reason string) {
	if !condition {
		fail(reason)
	}
}

//go:noinline
func printStats() {
	var s goroutine.Stats
	goroutine.ReadStats(&s)
	record("GOPROBE_STATS").field("live", s.Live).field("peak", s.Peak).field("created", s.Created).
		field("maxprocs", s.MaxProcs).field("workers", s.Workers).field("pinned", s.Pinned).field("idle", s.Idle).
		field("runnable", s.Runnable).field("parked", s.Parked).field("dispatches", s.Dispatches).
		field("steals", s.Steals).field("parks", s.Parks).field("wakes", s.Wakes).
		field("preempt_visits", s.PreemptVisits).field("preemptions", s.Preemptions).field("polls", s.Polls).
		field("waiters", s.Waiters).field("timers", s.Timers).
		field("stack_reserved", s.StackReserved).field("stack_pool", s.StackPool).end()
}

func now() int64 { return goroutine.Now() }

// pool runs the worker pool of the specification: four workers, a buffered
// job channel, a WaitGroup, and a result per job.
//
//go:noinline
func pool() {
	var wg sync.WaitGroup
	jobs := make(chan int, 16)
	results := make(chan int, 100)
	var perWorker [4]atomic.Uint64
	var threadsSeen atomic.Uint64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for job := range jobs {
				threadsSeen.Or(1 << goroutine.Thread())
				perWorker[worker].Add(1)
				results <- job * 2
			}
		}(i)
	}
	for i := 0; i < 100; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(results)
	sum, count := 0, 0
	for v := range results {
		sum += v
		count++
	}
	check(count == 100 && sum == 9900, "pool results")
	total := uint64(0)
	for i := range perWorker {
		total += perWorker[i].Load()
	}
	check(total == 100, "pool job count")
	record("GOPROBE_POOL").field("jobs", uint64(count)).field("sum", uint64(sum)).
		field("threads", uint64(popcount(threadsSeen.Load()))).end()
	ok("GOPROBE_POOL_OK")
}

func popcount(value uint64) int {
	count := 0
	for ; value != 0; value &= value - 1 {
		count++
	}
	return count
}

// smoke runs count goroutines that each wait, computing, until all are
// running at once, and records the threads they ran on.
//
//go:noinline
func smoke(count int) {
	if count < 1 || count > 15 {
		utility.Usage()
	}
	runtime.GOMAXPROCS(count)
	var running atomic.Int32
	var threadsSeen atomic.Uint64
	var together atomic.Int32
	var wg sync.WaitGroup
	started := now()
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			running.Add(1)
			deadline := now() + 5e9
			sum := uint64(0)
			for running.Load() < int32(count) && now() < deadline {
				for k := 0; k < 10000; k++ {
					sum += uint64(k) * uint64(k)
				}
			}
			if running.Load() >= int32(count) {
				together.Add(1)
			}
			threadsSeen.Or(1 << goroutine.Thread())
			if sum == 1 {
				println("unreachable")
			}
		}()
	}
	wg.Wait()
	threadCount := popcount(threadsSeen.Load())
	record("GOPROBE_SMOKE").field("goroutines", uint64(count)).field("together", uint64(together.Load())).
		field("threads", uint64(threadCount)).field("ns", uint64(now()-started)).end()
	check(together.Load() == int32(count), "goroutines did not all run at once")
	check(count == 1 || threadCount >= 2, "goroutines ran on one thread")
	ok("GOPROBE_SMOKE_OK")
}

// recovered runs fn and returns the panic it raised, or nil.
//
//go:noinline
func recovered(fn func()) (value any) {
	defer func() { value = recover() }()
	fn()
	return nil
}

//go:noinline
func channels() {
	// Unbuffered handoff between goroutines.
	unbuffered := make(chan int)
	go func() {
		for i := 0; i < 1000; i++ {
			unbuffered <- i
		}
		close(unbuffered)
	}()
	sum := 0
	for v := range unbuffered {
		sum += v
	}
	check(sum == 999*1000/2, "unbuffered sum")
	step("chan unbuffered")
	// Buffered: len and cap, closed receive.
	buffered := make(chan int, 3)
	buffered <- 1
	buffered <- 2
	check(len(buffered) == 2 && cap(buffered) == 3, "len and cap")
	close(buffered)
	a, okA := <-buffered
	b, okB := <-buffered
	c, okC := <-buffered
	check(a == 1 && okA && b == 2 && okB && c == 0 && !okC, "closed buffered receive")
	step("chan buffered")
	// Panics: send on a closed channel, close of a closed or nil channel.
	check(recovered(func() { buffered <- 3 }) != nil, "send on closed channel did not panic")
	check(recovered(func() { close(buffered) }) != nil, "double close did not panic")
	var nilChannel chan int
	check(recovered(func() { close(nilChannel) }) != nil, "close of nil channel did not panic")
	step("chan panics")
	check(recovered(func() {
		select {
		case buffered <- 3:
		default:
		}
	}) != nil, "select send on closed channel did not panic")
	// A recovered select must release the global select lock and its pins.
	probe := make(chan int, 1)
	probe <- 7
	select {
	case value := <-probe:
		check(value == 7, "select after recovered send")
	default:
		fail("select after recovered send was not ready")
	}
	select {
	case value, opened := <-buffered:
		check(value == 0 && !opened, "select closed receive")
	default:
		fail("closed receive was not ready")
	}
	selectClosedWhileParked()
	step("chan select panics")
	// Select: default, a nil case never ready, and exactly one case per
	// selection under concurrent senders and a close.
	select {
	case v := <-nilChannel:
		_ = v
		fail("nil channel was ready")
	default:
	}
	left, right, done := make(chan int), make(chan int), make(chan struct{})
	var sent atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 500; k++ {
				target := left
				if k%2 == 1 {
					target = right
				}
				select {
				case target <- 1:
					sent.Add(1)
				case <-done:
					return
				}
			}
		}(i)
	}
	received := int64(0)
	for received < 1000 {
		select {
		case v := <-left:
			received += int64(v)
		case v := <-right:
			received += int64(v)
		}
	}
	close(done)
	wg.Wait()
	// Whatever was sent was received; after close nothing more was sent.
	for {
		select {
		case <-left:
			received++
			continue
		case <-right:
			received++
			continue
		default:
		}
		break
	}
	check(sent.Load() == received, "select lost or duplicated a value")
	step("chan select")
	// A select that both receives a value and sees the close commits once.
	for round := 0; round < 200; round++ {
		values := make(chan int, 1)
		closing := make(chan struct{})
		go func() { values <- 1 }()
		go func() { close(closing) }()
		commits := 0
		for commits < 2 {
			select {
			case <-values:
				commits++
			case <-closing:
				commits++
				closing = nil
			}
		}
	}
	record("GOPROBE_CHAN").field("unbuffered_sum", uint64(sum)).field("select_values", uint64(received)).end()
	ok("GOPROBE_CHAN_OK")
}

//go:noinline
func selectClosedWhileParked() {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	var baseline goroutine.Stats
	goroutine.ReadStats(&baseline)
	closed, other := make(chan int), make(chan int, 1)
	result := make(chan bool, 1)
	go func() {
		result <- recovered(func() {
			select {
			case closed <- 1:
			case <-other:
			}
		}) != nil
	}()
	deadline := now() + 2e9
	for {
		var stats goroutine.Stats
		goroutine.ReadStats(&stats)
		if stats.Parked > baseline.Parked {
			break
		}
		check(now() < deadline, "select sender did not park")
		runtime.Gosched()
	}
	close(closed)
	check(<-result, "blocked select send did not panic after close")
	other <- 9
	select {
	case value := <-other:
		check(value == 9, "select after blocked-send recover")
	default:
		fail("select lock remained held after blocked-send recover")
	}
}

//go:noinline
func syncs() {
	var mu sync.Mutex
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 2000; k++ {
				mu.Lock()
				counter++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	check(counter == 16000, "mutex lost an increment")
	// RWMutex: readers in parallel, a writer alone.
	var rw sync.RWMutex
	var readers, maxReaders atomic.Int32
	shared := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 200; k++ {
				if i == 0 {
					rw.Lock()
					check(readers.Load() == 0, "writer ran with readers")
					shared++
					rw.Unlock()
				} else {
					rw.RLock()
					n := readers.Add(1)
					for {
						m := maxReaders.Load()
						if n <= m || maxReaders.CompareAndSwap(m, n) {
							break
						}
					}
					readers.Add(-1)
					rw.RUnlock()
				}
			}
		}(i)
	}
	wg.Wait()
	check(shared == 200, "rwmutex writer count")
	// Once, also when the first call panics.
	var once sync.Once
	calls := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			once.Do(func() { calls++ })
		}()
	}
	wg.Wait()
	check(calls == 1, "once ran more than once")
	var panicking sync.Once
	check(recovered(func() { panicking.Do(func() { panic("first") }) }) != nil, "once panic")
	ran := false
	panicking.Do(func() { ran = true })
	check(!ran, "once ran again after a panic")
	// Cond: Signal wakes one waiter, Broadcast the rest.
	var cmu sync.Mutex
	cond := sync.NewCond(&cmu)
	ready, woken := 0, 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmu.Lock()
			for ready == 0 {
				cond.Wait()
			}
			woken++
			ready--
			cmu.Unlock()
		}()
	}
	for i := 0; i < 5; i++ {
		goroutine.Sleep(2e6)
		cmu.Lock()
		ready++
		if i < 2 {
			cond.Signal()
		} else {
			cond.Broadcast()
		}
		cmu.Unlock()
	}
	wg.Wait()
	check(woken == 5, "cond lost a waiter")
	record("GOPROBE_SYNC").field("counter", uint64(counter)).field("max_readers", uint64(maxReaders.Load())).
		field("cond_woken", uint64(woken)).end()
	ok("GOPROBE_SYNC_OK")
}

//go:noinline
func timers() {
	runtime.GOMAXPROCS(1)
	// Sleeping goroutines keep no thread: on one permit, a computing
	// goroutine runs while a hundred sleep.
	var woke atomic.Int32
	var wg sync.WaitGroup
	started := now()
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(10+i%5) * time.Millisecond)
			woke.Add(1)
		}(i)
	}
	spins := uint64(0)
	for woke.Load() < 100 && now()-started < 5e9 {
		spins++
		runtime.Gosched()
	}
	wg.Wait()
	elapsed := now() - started
	check(woke.Load() == 100, "a sleeper did not wake")
	check(elapsed >= 10e6, "sleep returned early")
	// time.After, Timer.Stop, Timer.Reset, Ticker.
	select {
	case <-time.After(5 * time.Millisecond):
	case <-time.After(2 * time.Second):
		fail("the earlier timer did not fire first")
	}
	stopped := time.NewTimer(time.Hour)
	check(stopped.Stop(), "stop of a pending timer")
	reset := time.NewTimer(time.Hour)
	reset.Reset(time.Millisecond)
	<-reset.C
	ticker := time.NewTicker(2 * time.Millisecond)
	ticks := 0
	for ticks < 5 {
		<-ticker.C
		ticks++
	}
	ticker.Stop()
	// Context: a deadline and a cancel reach every waiter.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var expired atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ctx.Done()
			if ctx.Err() == context.DeadlineExceeded {
				expired.Add(1)
			}
		}()
	}
	wg.Wait()
	check(expired.Load() == 10, "context deadline did not reach every waiter")
	parent, cancelParent := context.WithCancel(context.Background())
	child, cancelChild := context.WithCancel(parent)
	defer cancelChild()
	cancelParent()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		fail("a cancel did not reach the child context")
	}
	runtime.GOMAXPROCS(runtime.NumCPU())
	record("GOPROBE_TIMERS").field("sleepers", 100).field("elapsed_ns", uint64(elapsed)).field("spins", spins).
		field("ticks", uint64(ticks)).end()
	ok("GOPROBE_TIMERS_OK")
}

//go:noinline
func spinWork(stop *atomic.Bool) uint64 {
	sum := uint64(1)
	for !stop.Load() {
		sum = sum*6364136223846793005 + 1442695040888963407
	}
	return sum
}

// fair runs, on one permit, a goroutine that loops without any call and
// another that counts; the counter must move.
//
//go:noinline
func fair() {
	runtime.GOMAXPROCS(1)
	var stop atomic.Bool
	var progress atomic.Int64
	var sink atomic.Uint64
	done := make(chan struct{})
	go func() {
		sink.Store(spinWork(&stop))
		close(done)
	}()
	// A second loop without calls holds live SSE registers across every
	// preemption that interrupts it.
	var vectorStop uint32
	vectorLost := make(chan uint64, 1)
	go func() { vectorLost <- sys.ProbeVectorSpin(&vectorStop) }()
	runtime.Gosched()
	started := now()
	var firstProgress int64
	go func() {
		for i := 0; i < 50; i++ {
			if firstProgress == 0 {
				firstProgress = now() - started
			}
			progress.Add(1)
			runtime.Gosched()
		}
	}()
	for progress.Load() < 50 && now()-started < 10e9 {
		goroutine.Sleep(1e6)
	}
	stop.Store(true)
	<-done
	atomic.StoreUint32(&vectorStop, 1)
	lost := <-vectorLost
	var s goroutine.Stats
	goroutine.ReadStats(&s)
	record("GOPROBE_FAIR").field("progress", uint64(progress.Load())).field("first_ns", uint64(firstProgress)).
		field("elapsed_ns", uint64(now()-started)).field("preemptions", s.Preemptions).field("vector_lost", lost).end()
	check(progress.Load() == 50, "a looping goroutine starved another")
	check(lost == 0, "a preemption lost the SSE registers of the code it interrupted")
	check(s.Preemptions > 0, "no goroutine was preempted")
	runtime.GOMAXPROCS(runtime.NumCPU())
	ok("GOPROBE_FAIR_OK")
}

//go:noinline
func panics() {
	results := make(chan string, 8)
	var deferred atomic.Int32
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() {
				deferred.Add(1)
				if r := recover(); r != nil {
					results <- "recovered"
					return
				}
				results <- "returned"
			}()
			if i%2 == 0 {
				panic("goprobe")
			}
		}(i)
	}
	recoveredCount, returned := 0, 0
	for i := 0; i < 8; i++ {
		switch <-results {
		case "recovered":
			recoveredCount++
		case "returned":
			returned++
		}
	}
	check(recoveredCount == 4 && returned == 4 && deferred.Load() == 8, "panic or defer in a goroutine")
	// The threads that ran panicking goroutines run others normally.
	var wg sync.WaitGroup
	var after atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			after.Add(1)
		}()
	}
	wg.Wait()
	check(after.Load() == 16, "goroutines after a panic")
	record("GOPROBE_PANIC").field("recovered", uint64(recoveredCount)).field("returned", uint64(returned)).end()
	ok("GOPROBE_PANIC_OK")
}

//go:noinline
func limit() {
	check(goroutine.SetLimit(32), "set limit")
	block := make(chan struct{})
	started := 0
	var wg sync.WaitGroup
	value := recovered(func() {
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-block
			}()
			started++
		}
	})
	check(value != nil, "the go statement past the limit did not panic")
	step("limit recovered")
	record("GOPROBE_LIMIT_CREATED").field("started", uint64(started)).end()
	wg.Done()
	close(block)
	step("limit channel closed")
	wg.Wait()
	step("limit workers done")
	record("GOPROBE_LIMIT").field("started", uint64(started)).field("limit", uint64(goroutine.Limit())).end()
	check(started > 0 && started < 32, "goroutines before the limit")
	check(goroutine.SetLimit(goroutine.DefaultLimit), "restore limit")
	// A goroutine that ended frees its slot: the limit is reusable.
	for round := 0; round < 4; round++ {
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() { wg.Done() }()
		}
		wg.Wait()
	}
	ok("GOPROBE_LIMIT_OK")
}

// node is a list cell the collector must keep while a goroutine holds it.
type node struct {
	next  *node
	value uint64
	pad   [6]uint64
}

func makeList(length int, seed uint64) *node {
	var head *node
	for i := 0; i < length; i++ {
		head = &node{next: head, value: seed + uint64(i)}
	}
	return head
}

//go:noinline
func listSum(head *node) uint64 {
	sum := uint64(0)
	for n := head; n != nil; n = n.next {
		sum += n.value
	}
	return sum
}

//go:noinline
func capturedList(length int, seed uint64) func() uint64 {
	head := makeList(length, seed)
	return func() uint64 { return listSum(head) }
}

func expectedSum(length int, seed uint64) uint64 {
	return uint64(length)*seed + uint64(length)*uint64(length-1)/2
}

// churn allocates and drops garbage so collections run.
//
//go:noinline
func churn(rounds int) {
	for i := 0; i < rounds; i++ {
		_ = makeList(64, 0)
	}
}

// roots keeps lists reachable only from parked, sleeping, waiting, and
// migrating goroutines, channel buffers, closures, and defer frames while
// count goroutines force collections, then checks every list.
//
//go:noinline
func roots(count int) {
	if count < 1 {
		count = 4
	}
	const length = 200
	baseline := arena.HeapStats()
	var wg sync.WaitGroup
	var bad atomic.Int32
	verify := func(head *node, seed uint64) {
		if listSum(head) != expectedSum(length, seed) {
			bad.Add(1)
		}
	}
	gate := make(chan struct{})
	var mu sync.Mutex
	mu.Lock()
	buffered := make(chan *node, 4)
	buffered <- makeList(length, 1000)
	pending := make(chan *node)
	// Parked on a channel receive, on a mutex, in select, asleep, in a
	// handle wait, and pending in a send: each holds its list on its stack.
	for kind := 0; kind < 6; kind++ {
		wg.Add(1)
		go func(kind int) {
			defer wg.Done()
			seed := uint64(kind+1) * 100000
			head := makeList(length, seed)
			defer verify(head, seed)
			switch kind {
			case 0:
				<-gate
			case 1:
				mu.Lock()
				mu.Unlock()
			case 2:
				select {
				case <-gate:
				case <-time.After(time.Hour):
				}
			case 3:
				time.Sleep(300 * time.Millisecond)
			case 4:
				receiver, send, streamStatus := sys.StreamCreate()
				check(streamStatus == kcall.StatusOK, "root wait stream")
				receive(receiver, now()+300e6, nil)
				sys.HandleClose(send)
				sys.HandleClose(receiver)
			case 5:
				pending <- makeList(length, 7000)
			}
			verify(head, seed)
		}(kind)
	}
	// A closure capture.
	closure := capturedList(length, 9000)
	// Collectors on several threads, with migrations between collections.
	var collectors sync.WaitGroup
	for i := 0; i < count; i++ {
		collectors.Add(1)
		go func(i int) {
			defer collectors.Done()
			for round := 0; round < 20; round++ {
				churn(50)
				runtime.GC()
				runtime.Gosched()
			}
		}(i)
	}
	collectors.Wait()
	close(gate)
	mu.Unlock()
	received := <-pending
	check(listSum(received) == expectedSum(length, 7000), "a pending send's value was lost")
	check(listSum(<-buffered) == expectedSum(length, 1000), "a channel buffer's value was lost")
	check(closure() == expectedSum(length, 9000), "a captured value was lost")
	wg.Wait()
	check(bad.Load() == 0, "a goroutine's list was lost")
	// Everything those goroutines held is garbage now; the stacks went
	// back to the pool.
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	after := arena.HeapStats()
	var s goroutine.Stats
	goroutine.ReadStats(&s)
	record("GOPROBE_GC").field("collectors", uint64(count)).field("collections", uint64(after.Collections-baseline.Collections)).
		field("live_before", baseline.Live).field("live_after", after.Live).field("goroutines", s.Live).
		field("stack_pool", s.StackPool).end()
	check(s.Live == 1, "goroutines outlived the check")
	check(after.Live < baseline.Live+256<<10, "garbage was not reclaimed")
	ok("GOPROBE_GC_OK")
}

var wakeMessage [ipc.WireHeaderSize]byte

// waitBuffer holds what a handle wait receives; one per goroutine that
// receives at once.
type waitBuffer struct {
	interests [kcall.WaitAnyMaxObjects]sys.Interest
	wire      [ipc.WireHeaderSize]byte
}

// receive takes a notification from a stream, parking the goroutine
// until data arrives, deadline passes, or cancel is canceled.
//
//go:noinline
func receive(channel handle.ID, deadline int64, cancel *goroutine.Cancel) kcall.Status {
	b := new(waitBuffer)
	b.interests[0] = sys.Interest{Handle: channel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
	for {
		if _, status := sys.StreamRead(channel, b.wire[:]); status != kcall.StatusResourceExhausted {
			return status
		}
		if _, status := goroutine.Wait(&b.interests, 1, deadline, cancel); status != kcall.StatusOK {
			return status
		}
	}
}

// receiveNative exercises the ordinary syscall API, without opting into
// goroutine.Wait. Its native wait hook must leave the only permit available.
//
//go:noinline
func receiveNative(channel handle.ID, deadline int64) kcall.Status {
	b := new(waitBuffer)
	b.interests[0] = sys.Interest{Handle: channel, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
	const perTick = int64(1000000000 / clock.TicksPerSecond)
	for {
		if _, status := sys.StreamRead(channel, b.wire[:]); status != kcall.StatusResourceExhausted {
			return status
		}
		remaining := deadline - now()
		if remaining <= 0 {
			return kcall.StatusTimeout
		}
		timeout := uint64((remaining + perTick - 1) / perTick)
		if _, status := sys.WaitAny(&b.interests, 1, timeout); status != kcall.StatusOK {
			return status
		}
	}
}

func sendNote(channel handle.ID) kcall.Status {
	count, status := sys.StreamWrite(channel, wakeMessage[:])
	if status == kcall.StatusOK && count != len(wakeMessage) {
		return kcall.StatusIOError
	}
	return status
}

// waits parks goroutines in handle waits while others run: a ping-pong
// through native streams, a timeout, a cancel, and a peer that closes.
//
//go:noinline
func waits() {
	if _, encoded := ipc.Encode(ipc.Message{ProtocolID: 0x474f50524f4245, Version: 1, Kind: ipc.KindNotification}, wakeMessage[:]); !encoded {
		fail("encode")
	}
	runtime.GOMAXPROCS(1)
	pingReceive, pingSend, pingStatus := sys.StreamCreate()
	pongReceive, pongSend, pongStatus := sys.StreamCreate()
	check(pingStatus == kcall.StatusOK && pongStatus == kcall.StatusOK, "create wait streams")
	const rounds = 500
	var spins atomic.Uint64
	stop := make(chan struct{})
	go func() {
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
	echoed := make(chan int)
	go func() {
		count := 0
		for count < rounds {
			if receiveNative(pingReceive, now()+2e9) != kcall.StatusOK {
				break
			}
			sendNote(pongSend)
			count++
		}
		echoed <- count
	}()
	started := now()
	for i := 0; i < rounds; i++ {
		check(sendNote(pingSend) == kcall.StatusOK, "ping send")
		if status := receive(pongReceive, now()+2e9, nil); status != kcall.StatusOK {
			record("GOPROBE_IO_FAILURE").field("round", uint64(i)).field("status", uint64(status)).end()
			fail("pong lost")
		}
	}
	check(<-echoed == rounds, "echo count")
	elapsed := now() - started
	// A timeout, a cancel, and a peer's close each end a wait exactly once.
	timeoutStarted := now()
	check(receive(pongReceive, now()+20e6, nil) == kcall.StatusTimeout, "wait timeout")
	waited := now() - timeoutStarted
	check(waited >= 20e6, "timeout came early")
	var cancel goroutine.Cancel
	canceled := make(chan kcall.Status)
	go func() { canceled <- receive(pongReceive, 0, &cancel) }()
	goroutine.Sleep(5e6)
	cancel.Cancel()
	check(<-canceled == goroutine.StatusCanceled, "wait cancel")
	closed := make(chan kcall.Status)
	go func() { closed <- receive(pingReceive, now()+2e9, nil) }()
	goroutine.Sleep(5e6)
	sys.HandleClose(pingSend)
	status := <-closed
	check(status == kcall.StatusPeerDied, "peer close did not end the wait")
	close(stop)
	sys.HandleClose(pingReceive)
	sys.HandleClose(pongSend)
	sys.HandleClose(pongReceive)
	runtime.GOMAXPROCS(runtime.NumCPU())
	record("GOPROBE_IO").field("rounds", rounds).field("round_ns", uint64(elapsed/rounds)).
		field("timeout_ns", uint64(waited)).field("spins", spins.Load()).end()
	check(spins.Load() > rounds, "the computing goroutine did not run while others waited")
	ok("GOPROBE_IO_OK")
}

var threadResults chan uint64

func pinnedWorker(argument uintptr) {
	sum := uint64(0)
	for v := range threadInputs {
		sum += v
		runtime.Gosched()
	}
	runtime.GC()
	threadResults <- sum + uint64(argument)
}

var threadInputs chan uint64

// threads runs thread.Start threads that receive from goroutines over
// channels, park, collect, and report back.
//
//go:noinline
func threads() {
	threadInputs = make(chan uint64)
	threadResults = make(chan uint64, 2)
	var ids [2]thread.ID
	for i := range ids {
		id, status := thread.Start(pinnedWorker, uintptr(i))
		check(status == kcall.StatusOK, "thread start")
		ids[i] = id
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for k := 0; k < 100; k++ {
				threadInputs <- uint64(k)
				if k%10 == 0 {
					churn(5)
				}
			}
		}(i)
	}
	wg.Wait()
	close(threadInputs)
	total := <-threadResults + <-threadResults
	for _, id := range ids {
		code, status := thread.Join(id)
		check(status == kcall.StatusOK && code == 0, "thread join")
	}
	check(total == 4*4950+1, "values between threads and goroutines")
	threadWakeCheck()
	record("GOPROBE_THREADS").field("total", total).end()
	ok("GOPROBE_THREADS_OK")
}

var nativeWake struct {
	ready   atomic.Bool
	status  atomic.Uint32
	mask    atomic.Uint64
	elapsed atomic.Int64
}

func wakeWaiter(uintptr) {
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: handle.ID(kcall.WaitAnyThread), Events: kcall.WaitReadable}
	sys.ThreadWakeTake()
	started := now()
	nativeWake.ready.Store(true)
	mask, status := sys.WaitAny(&interests, 1, uint64(clock.TicksPerSecond))
	nativeWake.mask.Store(mask)
	nativeWake.status.Store(uint32(status))
	nativeWake.elapsed.Store(now() - started)
}

func threadWakeCheck() {
	nativeWake.ready.Store(false)
	id, status := thread.Start(wakeWaiter, 0)
	check(status == kcall.StatusOK, "start native wake waiter")
	deadline := now() + 2e9
	for !nativeWake.ready.Load() {
		check(now() < deadline, "native wake waiter did not start")
		runtime.Gosched()
	}
	time.Sleep(20 * time.Millisecond)
	check(sys.ThreadWake(uint64(id)) == kcall.StatusOK, "wake native waiter")
	code, status := thread.Join(id)
	check(status == kcall.StatusOK && code == 0, "join native wake waiter")
	check(kcall.Status(nativeWake.status.Load()) == kcall.StatusOK && nativeWake.mask.Load() == 1, "native thread wait did not complete on wake")
	check(nativeWake.elapsed.Load() >= 20e6, "native thread wait returned before the wake")
}

//go:noinline
func recurse(depth int) int {
	var frame [512]byte
	index := depth % len(frame)
	// Keep the load after the recursive call: LLVM otherwise turns this
	// fixture into a constant-stack accumulator loop, even with noinline.
	volatile.StoreUint8(&frame[index], byte(depth))
	if depth == 0 {
		return int(volatile.LoadUint8(&frame[index]))
	}
	value := recurse(depth - 1)
	return value + int(volatile.LoadUint8(&frame[index]))
}

//go:noinline
func overflow() {
	done := make(chan int)
	go func() { done <- recurse(1 << 20) }()
	ok("GOPROBE_OVERFLOW_STARTED")
	<-done
	ok("GOPROBE_OVERFLOW_SURVIVED")
}

//go:noinline
func deadlock() {
	ok("GOPROBE_DEADLOCK_STARTED")
	block := make(chan int)
	go func() { <-block }()
	<-block
}

//go:noinline
func bench() {
	// Goroutine creation and exit.
	const creates = 2000
	var wg sync.WaitGroup
	started := now()
	for i := 0; i < creates; i++ {
		wg.Add(1)
		go func() { wg.Done() }()
		if i%100 == 99 {
			wg.Wait()
		}
	}
	wg.Wait()
	createNs := (now() - started) / creates
	// Channel ping-pong between two goroutines.
	const pings = 5000
	ping, pong := make(chan int), make(chan int)
	go func() {
		for v := range ping {
			pong <- v
		}
	}()
	started = now()
	for i := 0; i < pings; i++ {
		ping <- i
		<-pong
	}
	close(ping)
	pingNs := (now() - started) / pings
	// Select across four channels.
	var chans [4]chan int
	for i := range chans {
		chans[i] = make(chan int, 1)
	}
	started = now()
	for i := 0; i < pings; i++ {
		chans[i%4] <- i
		select {
		case <-chans[0]:
		case <-chans[1]:
		case <-chans[2]:
		case <-chans[3]:
		}
	}
	selectNs := (now() - started) / pings
	// Mutex handoff between two goroutines.
	var mu sync.Mutex
	counter := 0
	started = now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < pings; k++ {
				mu.Lock()
				counter++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	mutexNs := (now() - started) / (2 * pings)
	// Timer wake latency.
	const sleeps = 50
	var late int64
	for i := 0; i < sleeps; i++ {
		before := now()
		time.Sleep(time.Millisecond)
		late += now() - before - 1e6
	}
	record("GOPROBE_BENCH_RUNTIME").field("create_ns", uint64(createNs)).field("pingpong_ns", uint64(pingNs)).
		field("select_ns", uint64(selectNs)).field("mutex_ns", uint64(mutexNs)).
		field("sleep_late_ns", uint64(late/sleeps)).end()
	// CPU scaling: a fixed amount of work split over GOMAXPROCS workers.
	const work = 1 << 22
	var baseline int64
	for _, procs := range []int{1, 2, 4, 8} {
		if procs > runtime.NumCPU() {
			break
		}
		runtime.GOMAXPROCS(procs)
		started = now()
		var sink atomic.Uint64
		for i := 0; i < procs; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sum := uint64(1)
				for k := 0; k < work/procs; k++ {
					sum = sum*6364136223846793005 + uint64(k)
				}
				sink.Add(sum)
			}()
		}
		wg.Wait()
		elapsed := now() - started
		if procs == 1 {
			baseline = elapsed
		}
		record("GOPROBE_BENCH_SCALE").field("procs", uint64(procs)).field("ns", uint64(elapsed)).
			field("speedup_x100", uint64(baseline*100/elapsed)).end()
	}
	runtime.GOMAXPROCS(runtime.NumCPU())
	ok("GOPROBE_BENCH_OK")
}
