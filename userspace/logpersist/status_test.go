package logpersist

import (
	"strings"
	"testing"

	"goos/kernel/klog"
)

func TestAppendStatusCarriesEveryCounter(t *testing.T) {
	stats := Stats{Appended: 1, Overwritten: 2, Persisted: 3, Lost: 4, Failures: 5, Rotations: 6,
		RotationFailures: 7, Repaired: 8, RepairedBytes: 9}
	got := string(AppendStatus(nil, stats, Policy{SegmentBytes: 16 << 20, Segments: 4}))
	want := "appended 1\noverwritten 2\npersisted 3\nlost 4\nfailures 5\nrotations 6\n" +
		"rotation-failures 7\nrepaired 8\nrepaired-bytes 9\nsegment-bytes 16777216\nsegments 4\n"
	if got != want {
		t.Fatalf("status %q", got)
	}
	most := ^uint64(0)
	wide := AppendStatus(nil, Stats{most, most, most, most, most, most, most, most, most}, Policy{SegmentBytes: most, Segments: MaxSegments})
	if len(wide) > MaxStatus {
		t.Fatalf("widest status %d bytes", len(wide))
	}
}

// A persister's counters reach the status text: a drain of three records
// shows as appended and persisted.
func TestAppendStatusReflectsPersisterStats(t *testing.T) {
	var ring klog.Ring
	persister, _ := newPersister(t, &ring, newStore(), big)
	if !persister.Start(false) {
		t.Fatal("start")
	}
	appendRecords(&ring, 3)
	drainAll(t, persister)
	got := string(AppendStatus(nil, persister.Stats, persister.Policy()))
	for _, want := range []string{"appended 3\n", "persisted 3\n", "lost 0\n", "segments 4\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status %q lacks %q", got, want)
		}
	}
}
