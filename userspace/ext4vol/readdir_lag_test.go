package ext4vol

import (
	"testing"

	"goos/kernel/ramfs"
)

// laggingClient pages a directory the way the VFS can: it stops mid-batch
// when a short target fills and echoes the cursor of its last appended
// record, which lags the batch's last emitted record. The listing must stay
// complete and ordered (ADR 0208, ADR 0231).
func laggingClient(t *testing.T, volume *Volume, dir ramfs.NodeID, budget int) []string {
	t.Helper()
	var names []string
	cursor := 0
	for rounds := 0; ; rounds++ {
		if rounds > 10000 {
			t.Fatal("listing does not end")
		}
		target := make([]byte, budget)
		batch, status := volume.ReadDirBatch(dir, cursor, target)
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		var record ramfs.DirRecord
		for data := target[:batch.Used]; len(data) > 0; {
			size, ok := record.Decode(data)
			if !ok {
				t.Fatal("malformed record")
			}
			names = append(names, string(record.Name))
			data = data[size:]
		}
		cursor = batch.Next
		if batch.End {
			return names
		}
	}
}

// TestReadDirBatchLaggingCursor lists the 10000-name directory with a budget
// too small for a whole batch and requires every name once, in the same
// order as a full-budget listing: the seek must not skip or repeat entries.
func TestReadDirBatchLaggingCursor(t *testing.T) {
	volume := mountedMany(t)
	root, _ := volume.Root()
	many, _ := volume.OpenName(root, name(t, "many"))
	full := batchNames(t, volume, many, ramfs.MaxDirBatch, 0)
	lagged := laggingClient(t, volume, many, 200)
	if len(lagged) != 10000 || len(full) != 10000 {
		t.Fatalf("listed %d (lagged) and %d (full) names", len(lagged), len(full))
	}
	for index := range full {
		if full[index] != lagged[index] {
			t.Fatalf("entry %d: %q vs %q", index, full[index], lagged[index])
		}
	}
}
