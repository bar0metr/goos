//go:build goos_userspace

package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"goos/internal/logfmt"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

// logBatch is the snapshot a woken log waiter reads. A batch is 13 KiB, too
// large for a goroutine stack; logBatchLock gives it one reader at a time.
var (
	logBatch     [kcall.LogReadSize]byte
	logBatchLock sync.Mutex
)

const (
	logWaitComponent = "goprobe"
	logWaitMessage   = "logwait"
)

type logAnswer struct {
	sequence, waits, before, after uint64
	status                         kcall.Status
}

// logwait parks two goroutines on the same log cursor while a third counts
// on the only permit, then writes one record once both have parked. Each
// waiter must read that record, and the peer must have run during the wait
// that the record ended. PID 1 grants goprobe the log in validation builds.
func logwait(data startup.Data) {
	log, found := data.Handle("log")
	check(found, "no log capability")
	runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(runtime.NumCPU())
	_, next, status := sys.LogBounds(log)
	check(status == kcall.StatusOK, "read the log bounds")
	var digits [20]byte
	var storage [logfmt.MaxFields]byte
	fields, valid := logfmt.AppendField(storage[:0], "token", logfmt.AppendUint(digits[:0], uint64(now())))
	check(valid, "encode the log token")
	var baseline goroutine.Stats
	goroutine.ReadStats(&baseline)
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
	answers := make(chan logAnswer, 2)
	for waiter := 0; waiter < 2; waiter++ {
		go func() { answers <- awaitLogRecord(log, next, fields, &spins) }()
	}
	waitForWaiters(baseline.Waiters+2, "log waiters did not park")
	time.Sleep(20 * time.Millisecond)
	// A record from another process wakes the waiters early; they read past
	// it and park again. Write once both are parked again, so the record
	// lies past every waiter's cursor and ends a wait that parked.
	waitForWaiters(baseline.Waiters+2, "log waiters did not park again")
	var wire [logfmt.MaxWire]byte
	size, valid := logfmt.Encode(logfmt.Info, []byte(logWaitComponent), []byte(logWaitMessage), fields, wire[:])
	check(valid, "encode the log record")
	check(sys.LogWrite(log, wire[:size]) == kcall.StatusOK, "write the log record")
	first, second := <-answers, <-answers
	close(stop)
	<-spun
	for _, answer := range []logAnswer{first, second} {
		if answer.status != kcall.StatusOK {
			record("GOPROBE_FAILURE").field("status", uint64(answer.status)).field("waits", answer.waits).end()
			fail("a log waiter failed")
		}
		check(answer.after > answer.before, "the peer did not run while a log waiter was parked")
	}
	check(first.sequence == second.sequence && !logfmt.SequenceBefore(first.sequence, next), "log waiters read different records")
	var after goroutine.Stats
	goroutine.ReadStats(&after)
	check(after.Parks >= baseline.Parks+2, "log waiters did not park")
	check(after.Waiters == baseline.Waiters, "log waits stayed registered")
	record("GOPROBE_LOGWAIT").field("sequence", first.sequence).field("waits", first.waits+second.waits).
		field("peer", min(first.after-first.before, second.after-second.before)).
		field("polls", after.Polls-baseline.Polls).end()
	ok("GOPROBE_LOGWAIT_OK")
}

func waitForWaiters(want uint64, reason string) {
	deadline := now() + 2e9
	for {
		var stats goroutine.Stats
		goroutine.ReadStats(&stats)
		if stats.Waiters == want {
			return
		}
		check(now() < deadline, reason)
		runtime.Gosched()
	}
}

// awaitLogRecord waits on log from cursor until it reads the record that
// carries fields, and returns its sequence with the peer's count around the
// last wait.
func awaitLogRecord(log handle.ID, cursor uint64, fields []byte, spins *atomic.Uint64) logAnswer {
	interests := new([kcall.WaitAnyMaxObjects]sys.CursorInterest)
	answer := logAnswer{}
	for {
		interests[0] = sys.CursorInterest{Handle: log, Events: kcall.WaitReadable, Cursor: cursor}
		answer.before = spins.Load()
		mask, status := sys.WaitAnyCursor(interests, 1, uint64(5*clock.TicksPerSecond))
		answer.after = spins.Load()
		answer.waits++
		if status == kcall.StatusOK && mask != 1 {
			status = kcall.StatusCorruptData
		}
		if status != kcall.StatusOK {
			answer.status = status
			return answer
		}
		var found bool
		answer.sequence, cursor, found, answer.status = findLogRecord(log, cursor, fields)
		if found || answer.status != kcall.StatusOK {
			return answer
		}
	}
}

// findLogRecord reads log from cursor and returns the sequence of the
// goprobe record that carries fields, or the cursor past the batch.
func findLogRecord(log handle.ID, cursor uint64, fields []byte) (uint64, uint64, bool, kcall.Status) {
	logBatchLock.Lock()
	defer logBatchLock.Unlock()
	batch, status := sys.LogReadSequence(log, cursor, &logBatch)
	if status != kcall.StatusOK {
		return 0, cursor, false, status
	}
	for index := 0; index < batch.Count; index++ {
		entry, valid := batch.Record(index)
		if !valid {
			return 0, cursor, false, kcall.StatusCorruptData
		}
		if string(entry.Component) == logWaitComponent && string(entry.Message) == logWaitMessage && string(entry.Fields) == string(fields) {
			return batch.First + uint64(index), batch.Total, true, kcall.StatusOK
		}
	}
	return 0, batch.Total, false, kcall.StatusOK
}
