package logpersist

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"goos/kernel/klog"
)

// ringSource reads a real kernel ring the way LogReadSequence does.
type ringSource struct {
	ring   *klog.Ring
	copies [kcall.LogReadMax]klog.Copy
	// beforeRead runs before each read: a producer faster than the reader.
	beforeRead func()
	fail       bool
}

func (source *ringSource) Read(cursor uint64) (Batch, bool) {
	if source.fail {
		return Batch{}, false
	}
	if source.beforeRead != nil {
		source.beforeRead()
	}
	oldest, total, first, count := source.ring.Read(cursor, source.copies[:])
	return Batch{Oldest: oldest, Total: total, First: first, Count: count}, true
}

func (source *ringSource) Record(index int) (logfmt.Record, bool) {
	if index < 0 || index >= kcall.LogReadMax {
		return logfmt.Record{}, false
	}
	return source.copies[index].Record(), true
}

// memStore is a filesystem in memory with injected failures.
type memStore struct {
	files map[string][]byte
	open  string
	// writeBudget, when not negative, is how many more bytes reach the file
	// before a write fails part way.
	writeBudget int
	failReplace map[string]bool
	failOpen    bool
	replaces    int
}

func newStore() *memStore {
	return &memStore{files: map[string][]byte{}, writeBudget: -1, failReplace: map[string]bool{}}
}

func (store *memStore) Stat(path []byte) (uint64, bool, bool) {
	data, exists := store.files[string(path)]
	return uint64(len(data)), exists, true
}

func (store *memStore) ReadAt(path []byte, offset uint64, target []byte) (int, bool) {
	data, exists := store.files[string(path)]
	if !exists || offset > uint64(len(data)) {
		return 0, false
	}
	return copy(target, data[offset:]), true
}

func (store *memStore) Truncate(path []byte, size uint64) bool {
	data, exists := store.files[string(path)]
	if !exists || size > uint64(len(data)) {
		return false
	}
	store.files[string(path)] = data[:size:size]
	return true
}

func (store *memStore) Replace(from, to []byte) bool {
	if store.failReplace[string(to)] {
		return false
	}
	data, exists := store.files[string(from)]
	if !exists {
		return false
	}
	store.files[string(to)] = data
	delete(store.files, string(from))
	store.replaces++
	return true
}

func (store *memStore) OpenAppend(path []byte) (uint64, bool) {
	if store.failOpen || store.open != "" {
		return 0, false
	}
	store.open = string(path)
	return uint64(len(store.files[store.open])), true
}

func (store *memStore) Write(data []byte) bool {
	if store.open == "" {
		return false
	}
	if store.writeBudget >= 0 && len(data) > store.writeBudget {
		store.files[store.open] = append(store.files[store.open], data[:store.writeBudget]...)
		store.writeBudget = 0
		return false
	}
	if store.writeBudget >= 0 {
		store.writeBudget -= len(data)
	}
	store.files[store.open] = append(store.files[store.open], data...)
	return true
}

func (store *memStore) Close() { store.open = "" }

const livePath = "/state/system.log"

var big = Policy{SegmentBytes: DefaultSegmentBytes, Segments: DefaultSegments}

func appendRecords(ring *klog.Ring, count int) {
	for index := 0; index < count; index++ {
		total := ring.Total()
		ring.Append(logfmt.Record{Time: timekeeping.Time{Seconds: 1_800_000_000 + int64(total), Nanoseconds: uint32(total) * 1000}, Process: 7,
			Level: logfmt.Info, Component: []byte("burst"), Message: []byte("record"), Fields: logfmt.AppendUint([]byte("n="), total)})
	}
}

func newPersister(t *testing.T, ring *klog.Ring, store *memStore, policy Policy) (*Persister, *ringSource) {
	t.Helper()
	source := &ringSource{ring: ring}
	persister := new(Persister)
	if !persister.Init(source, store, livePath, policy) {
		t.Fatal("init refused")
	}
	return persister, source
}

func drainAll(t *testing.T, persister *Persister) {
	t.Helper()
	for pass := 0; pass < 1000; pass++ {
		switch persister.Drain() {
		case CaughtUp:
			return
		case Failed:
			t.Fatal("drain failed")
		}
	}
	t.Fatal("drain never caught up")
}

// sequences parses every line of the given files, oldest file first, and
// checks that each line is whole and well formed. Gap markers add to lost.
func sequences(t *testing.T, store *memStore, paths ...string) (records []uint64, lost uint64) {
	t.Helper()
	for _, path := range paths {
		data := store.files[path]
		if len(data) != 0 && data[len(data)-1] != '\n' {
			t.Fatalf("%s ends in a torn line", path)
		}
		for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			sequence, ok := SequenceOf(line)
			if !ok {
				t.Fatalf("%s: line without a sequence: %q", path, line)
			}
			var count uint64
			if n, _ := fmt.Sscanf(string(line), "[lost %d records]", &count); n == 1 {
				lost += count
				continue
			}
			if !bytes.Contains(line, []byte(" burst[7]: ")) {
				// A line from another writer or an earlier boot.
				continue
			}
			if _, ok := logfmt.TimeOf(line); !ok || !bytes.Contains(line, []byte(fmt.Sprintf("n=%d #%d", sequence, sequence))) {
				t.Fatalf("%s: corrupt line %q", path, line)
			}
			records = append(records, sequence)
		}
	}
	return records, lost
}

func requireIncreasing(t *testing.T, records []uint64) {
	t.Helper()
	for index := 1; index < len(records); index++ {
		if records[index] <= records[index-1] {
			t.Fatalf("sequence %d follows %d: duplicate or reordered", records[index], records[index-1])
		}
	}
}

func TestBurstWithHealthyStorageLosesNothing(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	persister, _ := newPersister(t, &ring, store, big)
	if !persister.Start(false) {
		t.Fatal("start")
	}
	appendRecords(&ring, 1000)
	drainAll(t, persister)
	records, lost := sequences(t, store, livePath)
	if len(records) != 1000 || records[0] != 0 || records[999] != 999 || lost != 0 {
		t.Fatalf("persisted %d lost %d", len(records), lost)
	}
	requireIncreasing(t, records)
	stats := persister.Stats
	if stats.Persisted != 1000 || stats.Lost != 0 || stats.Failures != 0 || stats.Appended != 1000 || stats.Overwritten != 0 {
		t.Fatalf("stats %+v", stats)
	}
	if persister.Cursor() != 1000 || store.open != "" {
		t.Fatal("cursor or open file left behind")
	}
}

// Records the ring overwrote before the service read them become one gap
// marker with the exact count, and the counters agree with the ring.
func TestProducerFasterThanPersistenceCountsExactOverwrites(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	persister, source := newPersister(t, &ring, store, big)
	appendRecords(&ring, 5000)
	if !persister.Start(false) {
		t.Fatal("start")
	}
	reads := 0
	source.beforeRead = func() {
		// Each read finds 500 new records: more than one batch, so the
		// ring overruns the reader again.
		if reads++; reads < 40 {
			appendRecords(&ring, 500)
		}
	}
	drainAll(t, persister)
	records, lost := sequences(t, store, livePath)
	requireIncreasing(t, records)
	total := ring.Total()
	if uint64(len(records))+lost != total || lost != persister.Stats.Lost || uint64(len(records)) != persister.Stats.Persisted {
		t.Fatalf("records %d lost %d total %d stats %+v", len(records), lost, total, persister.Stats)
	}
	if lost < 5000-klog.Capacity || records[len(records)-1] != total-1 {
		t.Fatalf("lost %d last %d", lost, records[len(records)-1])
	}
	first := bytes.SplitN(store.files[livePath], []byte("\n"), 2)[0]
	// The first read already finds the producer's first 500 records.
	if want := fmt.Sprintf("[lost %d records] #%d", 5500-klog.Capacity, 5500-klog.Capacity-1); string(first) != want {
		t.Fatalf("first line %q, want %q", first, want)
	}
}

// A write that fails part way leaves a torn line. The restarted service cuts
// it and continues after the last whole line, writing nothing twice.
func TestRestartMidBatchWritesNothingTwice(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	persister, _ := newPersister(t, &ring, store, big)
	persister.Start(false)
	appendRecords(&ring, 300)
	store.writeBudget = 5000
	if persister.Drain() != Failed {
		t.Fatal("injected failure did not fail the drain")
	}
	if data := store.files[livePath]; len(data) != 5000 || data[len(data)-1] == '\n' {
		t.Fatal("fixture did not leave a torn line")
	}
	appendRecords(&ring, 100)
	store.writeBudget = -1
	restarted, _ := newPersister(t, &ring, store, big)
	if !restarted.Start(true) {
		t.Fatal("restart")
	}
	if restarted.Stats.Repaired != 1 {
		t.Fatalf("torn line not repaired: %+v", restarted.Stats)
	}
	drainAll(t, restarted)
	records, lost := sequences(t, store, livePath)
	if len(records) != 400 || lost != 0 {
		t.Fatalf("records %d lost %d", len(records), lost)
	}
	requireIncreasing(t, records)
}

// The same service recovers from its own failed write without restarting.
func TestFailedWriteRecoversInPlace(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	persister, _ := newPersister(t, &ring, store, big)
	persister.Start(false)
	appendRecords(&ring, 200)
	store.writeBudget = 3333
	if persister.Drain() != Failed || persister.Stats.Failures != 1 {
		t.Fatal("injected failure")
	}
	store.writeBudget = -1
	drainAll(t, persister)
	records, _ := sequences(t, store, livePath)
	if len(records) != 200 {
		t.Fatalf("records %d", len(records))
	}
	requireIncreasing(t, records)
}

func TestTornFinalRecordFixture(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"torn tail", "[2026-01-01T00:00:00.000000Z] INFO  a[1]: one #0\n[2026-01-01T00:00:01.000000Z] INFO  a[1]: two #1\n[2026-01-01T00:00:02.0000", "[2026-01-01T00:00:00.000000Z] INFO  a[1]: one #0\n[2026-01-01T00:00:01.000000Z] INFO  a[1]: two #1\n"},
		{"only a torn line", "[2026-01-01T00:00:02.0000", ""},
		{"whole", "[2026-01-01T00:00:00.000000Z] INFO  a[1]: one #0\n", "[2026-01-01T00:00:00.000000Z] INFO  a[1]: one #0\n"},
		{"garbage longer than a tail", strings.Repeat("x", 3*tailSize), strings.Repeat("x", 3*tailSize-tailSize)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ring klog.Ring
			store := newStore()
			store.files[livePath] = []byte(tc.content)
			persister, _ := newPersister(t, &ring, store, big)
			persister.Start(false)
			if got := string(store.files[livePath]); got != tc.want {
				t.Fatalf("repaired to %q", got)
			}
			if wantRepairs := uint64(0); tc.content != tc.want {
				wantRepairs = 1
				if persister.Stats.Repaired != wantRepairs || persister.Stats.RepairedBytes != uint64(len(tc.content)-len(tc.want)) {
					t.Fatalf("stats %+v", persister.Stats)
				}
			}
			appendRecords(&ring, 3)
			drainAll(t, persister)
			lines := strings.Split(strings.TrimSuffix(string(store.files[livePath][len(tc.want):]), "\n"), "\n")
			if len(lines) != 3 || !strings.HasSuffix(lines[0], " #0") {
				t.Fatalf("appended %q", lines)
			}
		})
	}
}

// After a reboot the file's sequences belong to the earlier boot: the
// service starts at the ring's start rather than skipping to them.
func TestRebootIgnoresTheEarlierBootsSequences(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	store.files[livePath] = []byte("[2026-01-01T00:00:00.000000Z] INFO  a[1]: earlier boot #5\n")
	appendRecords(&ring, 20)
	persister, _ := newPersister(t, &ring, store, big)
	persister.Start(false)
	drainAll(t, persister)
	records, _ := sequences(t, store, livePath)
	if len(records) != 20 || records[0] != 0 {
		t.Fatalf("records %v", records)
	}
	// A restart within the boot whose tail does not match the ring does
	// not skip either.
	store.files[livePath] = []byte("[2026-01-01T00:00:00.000000Z] INFO  a[1]: earlier boot #5\n")
	again, _ := newPersister(t, &ring, store, big)
	again.Start(true)
	if again.Cursor() != 0 {
		t.Fatalf("a foreign tail moved the cursor to %d", again.Cursor())
	}
}

func segmentPath(index int) string {
	if index == 0 {
		return livePath
	}
	return fmt.Sprintf("%s.%d", livePath, index)
}

func TestFourRotationsInOneRun(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	policy := Policy{SegmentBytes: MinSegmentBytes, Segments: 4}
	persister, _ := newPersister(t, &ring, store, policy)
	persister.Start(false)
	appendRecords(&ring, 100)
	drainAll(t, persister)
	if persister.Stats.Rotations < 4 || persister.Stats.RotationFailures != 0 {
		t.Fatalf("stats %+v", persister.Stats)
	}
	for index := 0; index < 4; index++ {
		size := len(store.files[segmentPath(index)])
		if size == 0 || uint64(size) > policy.SegmentBytes {
			t.Fatalf("segment %d holds %d bytes", index, size)
		}
	}
	if _, exists := store.files[segmentPath(4)]; exists {
		t.Fatal("a fifth segment")
	}
	records, _ := sequences(t, store, segmentPath(3), segmentPath(2), segmentPath(1), livePath)
	requireIncreasing(t, records)
	for index := 1; index < len(records); index++ {
		if records[index] != records[index-1]+1 {
			t.Fatalf("segments are not contiguous at %d", records[index])
		}
	}
	if records[len(records)-1] != 99 {
		t.Fatal("the newest record is not in the live file")
	}
}

// A rotation that cannot move a segment stops there. The previous segment
// survives under its own name or the next one, and the live file keeps every
// record past its bound.
func TestRotationFailureKeepsThePreviousSegment(t *testing.T) {
	for _, failing := range []string{segmentPath(1), segmentPath(2)} {
		t.Run(failing, func(t *testing.T) {
			var ring klog.Ring
			store := newStore()
			previous := []byte("[2026-01-01T00:00:00.000000Z] INFO  a[1]: previous segment #3\n")
			store.files[segmentPath(1)] = previous
			store.failReplace[failing] = true
			persister, _ := newPersister(t, &ring, store, Policy{SegmentBytes: MinSegmentBytes, Segments: 4})
			persister.Start(false)
			appendRecords(&ring, 40)
			drainAll(t, persister)
			kept := false
			for index := 1; index < 4; index++ {
				kept = kept || bytes.Equal(store.files[segmentPath(index)], previous)
			}
			if !kept {
				t.Fatal("the previous segment is gone")
			}
			if persister.Stats.Rotations != 0 || persister.Stats.RotationFailures == 0 {
				t.Fatalf("stats %+v", persister.Stats)
			}
			records, lost := sequences(t, store, livePath)
			if len(records) != 40 || lost != 0 {
				t.Fatalf("live file holds %d records", len(records))
			}
			// Once the filesystem recovers, the next pass rotates.
			delete(store.failReplace, failing)
			appendRecords(&ring, 1)
			drainAll(t, persister)
			if persister.Stats.Rotations == 0 {
				t.Fatal("rotation never retried")
			}
		})
	}
}

func TestUnavailableStorageKeepsTheCursor(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	persister, _ := newPersister(t, &ring, store, big)
	persister.Start(false)
	appendRecords(&ring, 10)
	store.failOpen = true
	if persister.Drain() != Failed || persister.Cursor() != 0 || persister.Stats.Failures != 1 {
		t.Fatal("failure moved the cursor")
	}
	store.failOpen = false
	drainAll(t, persister)
	if records, _ := sequences(t, store, livePath); len(records) != 10 {
		t.Fatalf("records %d", len(records))
	}
}

func TestSkipWhileDisabledWritesNothing(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	persister, _ := newPersister(t, &ring, store, big)
	persister.Start(false)
	appendRecords(&ring, 10)
	persister.Skip()
	appendRecords(&ring, 2)
	drainAll(t, persister)
	if records, lost := sequences(t, store, livePath); len(records) != 2 || records[0] != 10 || lost != 0 {
		t.Fatalf("records %v lost %d", records, lost)
	}
}

func TestPolicyBounds(t *testing.T) {
	if !DefaultPolicy(false).Valid() || !DefaultPolicy(true).Valid() {
		t.Fatal("a default policy is invalid")
	}
	if DefaultPolicy(false).SegmentBytes*uint64(DefaultPolicy(false).Segments) != 64<<20 {
		t.Fatal("release history is not 64 MiB")
	}
	for _, policy := range []Policy{{MinSegmentBytes - 1, 4}, {MaxSegmentBytes + 1, 4}, {DefaultSegmentBytes, 1}, {DefaultSegmentBytes, MaxSegments + 1}} {
		if policy.Valid() {
			t.Fatalf("%+v accepted", policy)
		}
	}
	var persister Persister
	if persister.Init(&ringSource{}, newStore(), livePath, Policy{}) {
		t.Fatal("init took an invalid policy")
	}
	persister.Init(&ringSource{}, newStore(), livePath, big)
	if persister.SetPolicy(Policy{SegmentBytes: 10, Segments: 4}) || persister.Policy() != big {
		t.Fatal("an invalid policy replaced a valid one")
	}
}

func TestSequenceOf(t *testing.T) {
	for line, want := range map[string]int64{
		"[x] INFO a[1]: m #12":    12,
		"[lost 3 records] #0":     0,
		"no trailer":              -1,
		"#5":                      -1,
		"text#5":                  -1,
		"trailing #":              -1,
		"a #99999999999999999999": -1,
		"a #18446744073709551615": -2,
	} {
		got, ok := SequenceOf([]byte(line))
		switch {
		case want == -1 && ok, want >= 0 && (!ok || got != uint64(want)), want == -2 && (!ok || got != ^uint64(0)):
			t.Fatalf("%q: %d %v", line, got, ok)
		}
	}
}

func TestDrainDoesNotAllocate(t *testing.T) {
	var ring klog.Ring
	store := newStore()
	persister, _ := newPersister(t, &ring, store, big)
	persister.Start(false)
	appendRecords(&ring, 1)
	drainAll(t, persister)
	if allocations := testing.AllocsPerRun(50, func() { persister.Drain() }); allocations != 0 {
		t.Fatalf("an idle drain allocated %v times", allocations)
	}
}
