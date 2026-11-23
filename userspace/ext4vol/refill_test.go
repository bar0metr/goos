package ext4vol

import (
	"testing"

	"goos/kernel/ramfs"
)

// TestRefillCounts checks the cross-call batch reuse: a listing should
// refill about once per MaxReaddirPrefetchEntries entries, not once per
// exchange.
func TestRefillCounts(t *testing.T) {
	volume := mountedMany(t)
	root, _ := volume.Root()
	many, _ := volume.OpenName(root, name(t, "many"))
	for _, budget := range []int{251, 2000, ramfs.MaxDirBatch} {
		volume.ListingCalls, volume.ListingRefills = 0, 0
		cursor := 0
		for rounds := 0; ; rounds++ {
			if rounds > 20000 {
				t.Fatal("listing does not end")
			}
			target := make([]byte, budget)
			batch, status := volume.ReadDirBatch(many, cursor, target)
			if status != ramfs.StatusOK {
				t.Fatal(status)
			}
			cursor = batch.Next
			if batch.End {
				break
			}
		}
		t.Logf("budget=%4d calls=%d refills=%d", budget, volume.ListingCalls, volume.ListingRefills)
		if volume.ListingRefills*10 > volume.ListingCalls*4 {
			t.Fatalf("budget %d: %d refills for %d calls, batch reuse broken", budget, volume.ListingRefills, volume.ListingCalls)
		}
	}
}
