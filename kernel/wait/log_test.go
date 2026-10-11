package wait

import (
	"goos/internal/logfmt"
	"goos/kernel/klog"
	"goos/kernel/sched"
	"testing"
)

func logEntry(object, cursor uint64) Entry {
	return Entry{Key: Key{Kind: KindLog, Object: object}, Events: Readable, Cursor: cursor}
}

func TestLogPublicationIsCursorAwareAndIndependent(t *testing.T) {
	var table Table
	var out [sched.MaxTasks]Completion
	for i, cursor := range []uint64{3, 4, 5, 7} {
		if !table.Begin(task(uint32(i), 1), []Entry{logEntry(10, cursor)}, 0) {
			t.Fatal("begin")
		}
	}
	if n := table.PublishLog(10, 4, out[:]); n != 1 || out[0].Task != task(0, 1) {
		t.Fatalf("woken %d %+v", n, out[0])
	}
	if n := table.PublishLog(10, 4, out[:]); n != 0 {
		t.Fatal("same cursor publication woke reader")
	}
	if n := table.Publish(Key{Kind: KindLog, Object: 10}, Readable, out[:]); n != 0 {
		t.Fatal("non-sequenced publication woke reader")
	}
	if n := table.PublishLog(10, 6, out[:]); n != 2 {
		t.Fatalf("remaining wake %d", n)
	}
	if table.Count() != 1 {
		t.Fatal("future cursor not idle")
	}
	if !table.Cancel(task(3, 1)) {
		t.Fatal("future reader cancel")
	}
}

func TestLogWaitSharesTerminalCancellationAndKeepsLegacyEntries(t *testing.T) {
	var table Table
	var out [sched.MaxTasks]Completion
	entries := []Entry{logEntry(10, 50), entry(KindChannel, 20, 0, Readable|PeerClosed)}
	id := task(0, 1)
	if !table.Begin(id, entries, 0) {
		t.Fatal("begin combined wait")
	}
	if n := table.PublishLog(10, 50, out[:]); n != 0 || table.Count() != 1 {
		t.Fatal("idle log woke")
	}
	if n := table.Publish(entries[1].Key, Readable, out[:]); n != 1 {
		t.Fatal("terminal did not cancel wait")
	}
	result, ok := table.Take(id)
	if !ok || result.ReadyMask != 2 || table.Count() != 0 || table.RegistrationCount() != 0 {
		t.Fatalf("cancel %+v %v", result, ok)
	}
	if n := table.PublishLog(10, 51, out[:]); n != 0 {
		t.Fatal("cancelled registration retained")
	}
	if table.Begin(task(1, 1), []Entry{{Key: entries[1].Key, Events: Readable, Cursor: 9}}, 0) {
		t.Fatal("nonlog cursor accepted")
	}
	if table.Begin(task(1, 1), []Entry{{Key: entries[0].Key, Events: Writable, Cursor: 9}}, 0) {
		t.Fatal("log write readiness accepted")
	}
}

func TestLogRegistrationBeforeFinalRecheckClosesAppendRaces(t *testing.T) {
	for _, beforeBegin := range []bool{true, false} {
		var ring klog.Ring
		var table Table
		var out [sched.MaxTasks]Completion
		id := task(0, 1)
		cursor := ring.Total()
		publish := func() {
			total := ring.Append(logfmt.Record{Level: logfmt.Info, Component: []byte("test"), Message: []byte("wake")})
			table.PublishLog(10, total, out[:])
		}
		if beforeBegin {
			publish()
		}
		if !table.Begin(id, []Entry{logEntry(10, cursor)}, 0) {
			t.Fatal("begin")
		}
		if !beforeBegin {
			done := make(chan struct{})
			go func() { publish(); close(done) }()
			<-done
		}
		// This is the kernel's final observation after registration. If append
		// preceded Begin, it cannot publish to this task but the recheck sees it.
		if cursor >= ring.Total() {
			t.Fatal("lost append at recheck")
		}
		if beforeBegin {
			if !table.Cancel(id) {
				t.Fatal("cancel after ready recheck")
			}
		} else if result, ok := table.Take(id); !ok || result.ReadyMask != 1 {
			t.Fatalf("registered append lost: %+v %v", result, ok)
		}
	}
}

func TestLogPublicationAcrossSequenceCounterWrap(t *testing.T) {
	var table Table
	var out [sched.MaxTasks]Completion
	for i, cursor := range []uint64{^uint64(0) - 1, 2, 3} {
		if !table.Begin(task(uint32(i), 1), []Entry{logEntry(10, cursor)}, 0) {
			t.Fatal("begin")
		}
	}
	if n := table.PublishLog(10, 2, out[:]); n != 1 || out[0].Task != task(0, 1) {
		t.Fatalf("wrapped wake %d %+v", n, out[0])
	}
	if n := table.PublishLog(10, ^uint64(0), out[:]); n != 0 {
		t.Fatal("delayed pre-wrap publisher woke newer cursor")
	}
	if n := table.PublishLog(10, 3, out[:]); n != 1 || out[0].Task != task(1, 1) {
		t.Fatal("independent wrapped cursor")
	}
}
