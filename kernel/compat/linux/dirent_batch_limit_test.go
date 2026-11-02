package linux

import (
	"testing"

	"goos/internal/limits"
	"goos/kernel/ramfs"
)

// The limit never cuts off an entry the buffer can take, whatever the names.
func TestDirentBatchLimitHoldsWhatFits(t *testing.T) {
	for size := 1; size <= limits.NameMax; size++ {
		record := ramfs.DirRecordSize(size)
		dirent := DirentSize(size)
		for free := dirent; free < ramfs.MaxDirBatch; free += dirent {
			if limit := DirentBatchLimit(uint64(free)); int(limit)/record < min(free/dirent, ramfs.MaxDirBatch/record) {
				t.Fatalf("name %d, %d free: limit %d holds fewer records than fit", size, free, limit)
			}
		}
	}
}

func TestDirentBatchLimitIsBounded(t *testing.T) {
	if limit := DirentBatchLimit(1 << 40); limit != ramfs.MaxDirBatch {
		t.Fatalf("large buffer: %d", limit)
	}
	if limit := DirentBatchLimit(512); limit != 1536 {
		t.Fatalf("512 free: %d", limit)
	}
}
