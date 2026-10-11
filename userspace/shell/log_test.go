package shell

import (
	"encoding/binary"
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"strings"
	"testing"
)

func parseLogText(t *testing.T, line string) (LogOptions, bool) {
	t.Helper()
	var storage Storage
	invocation, valid := ParseInvocation([]byte(line), &storage)
	if !valid {
		return LogOptions{}, false
	}
	return ParseLog(invocation)
}

func TestLogOptionsAndExactFilters(t *testing.T) {
	for _, line := range []string{"log", "log -f", "log -n 0", "log -n 18446744073709551615", "log -f -n 5 --level info --component test", "log --component test --level error -n 3 -f"} {
		if _, valid := parseLogText(t, line); !valid {
			t.Fatalf("rejected %s", line)
		}
	}
	for _, line := range []string{"log -n", "log -n -1", "log -n x", "log -n 18446744073709551616", "log -n 1 -n 2", "log -f -f", "log --level", "log --level INFO", "log --level fatal", "log --component", "log --component ''", "log --component 'bad name'", "log --component " + strings.Repeat("a", 17), "log unknown", "log -F"} {
		if _, valid := parseLogText(t, line); valid {
			t.Fatalf("accepted %s", line)
		}
	}
	options, _ := parseLogText(t, "log -n 999 --level info --component test")
	if options.Limit != kcall.LogReadMax || options.Level != logfmt.Info {
		t.Fatal("count clamp or level")
	}
	for _, level := range []logfmt.Level{logfmt.Debug, logfmt.Info, logfmt.Warn, logfmt.Error} {
		if options.Matches(logfmt.Record{Level: level, Component: []byte("test")}) != (level == logfmt.Info) {
			t.Fatal("level is not exact")
		}
	}
	for _, component := range []string{"tes", "testing", "Test", "x-test"} {
		if options.Matches(logfmt.Record{Level: logfmt.Info, Component: []byte(component)}) {
			t.Fatal("component is not exact")
		}
	}
}

func logFixture(t *testing.T, wire *[kcall.LogReadSize]byte, oldest, total, cursor uint64) sys.LogBatch {
	t.Helper()
	clear(wire[:])
	first := cursor
	if logfmt.SequenceBefore(first, oldest) {
		first = oldest
		wire[28] = 1
	}
	binary.LittleEndian.PutUint64(wire[0:8], oldest)
	binary.LittleEndian.PutUint64(wire[8:16], total)
	binary.LittleEndian.PutUint64(wire[16:24], first)
	if logfmt.SequenceBefore(first, total) {
		binary.LittleEndian.PutUint32(wire[24:28], uint32(total-first))
		for index := 0; uint64(index) < total-first; index++ {
			sequence := first + uint64(index)
			level := logfmt.Info
			if sequence%2 != 0 {
				level = logfmt.Warn
			}
			component := []byte("test")
			if sequence%3 == 0 {
				component = []byte("other")
			}
			offset := kcall.LogReadHeader + index*kcall.LogReadRecordSize
			size, valid := logfmt.EncodeRecord(logfmt.Record{Time: timekeeping.Time{Seconds: int64(sequence)}, Process: sequence, Level: level, Component: component, Message: []byte("event")}, wire[offset+2:offset+kcall.LogReadRecordSize])
			if !valid {
				t.Fatal("fixture record")
			}
			binary.LittleEndian.PutUint16(wire[offset:offset+2], uint16(size))
		}
	}
	batch, valid := sys.DecodeLogBatch(wire[:], cursor)
	if !valid {
		t.Fatalf("fixture window %d %d %d", oldest, total, cursor)
	}
	return batch
}

func TestLogNewestMatchingSelectionIsChronological(t *testing.T) {
	var wire [kcall.LogReadSize]byte
	batch := logFixture(t, &wire, 20, 40, 0)
	var selected [kcall.LogReadMax]uint8
	options, _ := parseLogText(t, "log -n 3 --level info --component test")
	count := SelectLog(batch, options, &selected)
	if count != 3 {
		t.Fatalf("count %d", count)
	}
	for index, expected := range []int64{32, 34, 38} {
		record, _ := batch.Record(int(selected[index]))
		if record.Time.Seconds != expected {
			t.Fatalf("record %d got %d expected %d", index, record.Time.Seconds, expected)
		}
	}
	options.Limit = 0
	if SelectLog(batch, options, &selected) != 0 {
		t.Fatal("-n 0 emitted backlog")
	}
	options.Limit = 64
	options.Component = []byte("absent")
	if SelectLog(batch, options, &selected) != 0 {
		t.Fatal("no-match selection")
	}
}

func TestFollowBlocksWhenIdleAndCancelsAtTerminalWake(t *testing.T) {
	var wire [kcall.LogReadSize]byte
	var selected [kcall.LogReadMax]uint8
	reads, waits, writes := 0, 0, 0
	interrupted := false
	status := RunLog(LogOptions{Limit: 0, Follow: true}, LogHooks{
		Read: func(cursor uint64, snapshot bool) (sys.LogBatch, bool) {
			reads++
			return logFixture(t, &wire, 0, 5, cursor), true
		},
		Write: func(logfmt.Record) bool { writes++; return true },
		Gap:   func(uint64) bool { t.Fatal("unexpected gap"); return false },
		Check: func() (uint64, bool) { return 130, interrupted },
		Wait: func(cursor uint64) bool {
			waits++
			if cursor != 5 {
				t.Fatal("wait without consumed cursor")
			}
			interrupted = true
			return true
		},
	}, &selected)
	if status != 130 || reads != 2 || waits != 1 || writes != 0 {
		t.Fatalf("status %d reads %d waits %d writes %d", status, reads, waits, writes)
	}
}

func TestFollowAppendWrapGapAndFiltering(t *testing.T) {
	var wire [kcall.LogReadSize]byte
	var selected [kcall.LogReadMax]uint8
	total, waits, gap := uint64(5), 0, uint64(0)
	var written []int64
	options, _ := parseLogText(t, "log -f -n 1 --level info --component test")
	status := RunLog(options, LogHooks{
		Read: func(cursor uint64, snapshot bool) (sys.LogBatch, bool) {
			oldest := uint64(0)
			if total > 64 {
				oldest = total - 64
			}
			if snapshot {
				cursor = oldest
			}
			return logFixture(t, &wire, oldest, total, cursor), true
		},
		Write: func(record logfmt.Record) bool { written = append(written, record.Time.Seconds); return true },
		Gap:   func(lost uint64) bool { gap += lost; return true },
		Check: func() (uint64, bool) { return 130, waits == 3 },
		Wait: func(cursor uint64) bool {
			waits++
			switch waits {
			case 1:
				total = 9
			case 2:
				total = 80
			}
			return true
		},
	}, &selected)
	if status != 130 || gap != 7 || waits != 3 || len(written) < 3 || written[0] != 4 || written[1] != 8 {
		t.Fatalf("status %d gap %d waits %d records %v", status, gap, waits, written)
	}
	previous := int64(-1)
	for _, value := range written {
		if value <= previous || value%2 != 0 || value%3 == 0 {
			t.Fatalf("duplicate/out-of-order/nonmatching %v", written)
		}
		previous = value
	}
}

func TestFollowCancellationBoundsBatchesAndPropagatesErrors(t *testing.T) {
	var wire [kcall.LogReadSize]byte
	var selected [kcall.LogReadMax]uint8
	writes := 0
	status := RunLog(LogOptions{Limit: 64, Follow: true}, LogHooks{
		Read: func(cursor uint64, snapshot bool) (sys.LogBatch, bool) {
			return logFixture(t, &wire, 0, 64, cursor), true
		},
		Write: func(logfmt.Record) bool { writes++; return true },
		Check: func() (uint64, bool) { return 130, writes >= 1 },
	}, &selected)
	if status != 130 || writes != LogBatchWork {
		t.Fatalf("status %d cancellation after %d records", status, writes)
	}
	for _, fail := range []string{"read", "write", "wait"} {
		hooks := LogHooks{Read: func(cursor uint64, snapshot bool) (sys.LogBatch, bool) {
			return logFixture(t, &wire, 0, 1, cursor), fail != "read"
		}, Write: func(logfmt.Record) bool { return fail != "write" }, Check: func() (uint64, bool) { return 0, false }, Wait: func(uint64) bool { return false }}
		if got := RunLog(LogOptions{Limit: 64, Follow: true}, hooks, &selected); got != 1 {
			t.Fatalf("%s failure status %d", fail, got)
		}
	}
}

func TestFollowInitialSnapshotAndCursorCrossSequenceWrap(t *testing.T) {
	var wire [kcall.LogReadSize]byte
	var selected [kcall.LogReadMax]uint8
	total, waits := ^uint64(0)-2, 0
	var written []int64
	status := RunLog(LogOptions{Limit: 2, Follow: true}, LogHooks{
		Read: func(cursor uint64, snapshot bool) (sys.LogBatch, bool) {
			oldest := total - 64
			if snapshot {
				cursor = oldest
			}
			return logFixture(t, &wire, oldest, total, cursor), true
		},
		Write: func(record logfmt.Record) bool { written = append(written, record.Time.Seconds); return true },
		Gap:   func(uint64) bool { t.Fatal("unexpected wrap gap"); return false },
		Check: func() (uint64, bool) { return 130, waits == 2 },
		Wait: func(cursor uint64) bool {
			if cursor != total {
				t.Fatal("wrong wrap wait cursor")
			}
			waits++
			total = 2
			return true
		},
	}, &selected)
	expected := []int64{-5, -4, -3, -2, -1, 0, 1}
	if status != 130 || len(written) != len(expected) {
		t.Fatalf("wrapped follow status%d records%v", status, written)
	}
	for i := range expected {
		if written[i] != expected[i] {
			t.Fatalf("wrapped chronology %v", written)
		}
	}
}
