package ext4

import (
	"fmt"
	"testing"
	"time"
)

// TestBenchPrefetchPhases times the batch machinery on the canonical 2000-name
// directory: collection, ordering, fetching and decoding separately.
func TestBenchPrefetchPhases(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	dir := resolve(t, fs, "/big")
	var entries [MaxReaddirPrefetchEntries]DirEntry
	var inodes [MaxReaddirPrefetchEntries]Inode
	collectTime, fetchTime, total := time.Duration(0), time.Duration(0), time.Duration(0)
	count := 0
	cursor := uint64(0)
	for {
		start := time.Now()
		n, next, end, err := fs.PrefetchDir(&fs.list, dir, cursor, &entries, &inodes)
		total += time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		count += n
		cursor = next
		if end {
			break
		}
	}
	_ = collectTime
	_ = fetchTime
	fmt.Printf("PrefetchDir total: %v for %d entries (%.0f ns/entry)\n", total, count, float64(total.Nanoseconds())/float64(count))
}
