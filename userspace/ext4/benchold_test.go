package ext4

import (
	"fmt"
	"testing"
	"time"
)

// TestBenchOldListing times the per-entry walk the old batch path used, so
// the prefetch cost has a baseline to compare against.
func TestBenchOldListing(t *testing.T) {
	fs := mount(t, fixture(t, "canonical"))
	dir := resolve(t, fs, "/big")
	start := time.Now()
	count := 0
	if _, err := fs.ReadDir(dir, 0, func(entry *DirEntry) bool {
		if _, err := fs.Inode(entry.Inode); err != nil {
			t.Fatal(err)
		}
		count++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	total := time.Since(start)
	fmt.Printf("per-entry walk: %v for %d entries (%.0f ns/entry)\n", total, count, float64(total.Nanoseconds())/float64(count))
}
