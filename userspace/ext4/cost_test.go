package ext4

import (
	"fmt"
	"testing"
)

// costWorkload names a workload and runs count operations of it.
type costWorkload struct {
	name string
	// run performs count operations and calls after each one.
	run func(t testing.TB, fs *FS, count int, after func())
}

var costWorkloads = []costWorkload{
	{"create", func(t testing.TB, fs *FS, count int, after func()) {
		dir := ok(fs.Mkdir(root(t, fs), []byte("cost"), 0o755, 0, 0))
		for index := 0; index < count; index++ {
			ok(fs.Create(dir, []byte(fmt.Sprintf("file-%05d", index)), ModeRegular|0o644, 0, 0))
			after()
		}
	}},
	{"append-4k", func(t testing.TB, fs *FS, count int, after func()) {
		file := ok(fs.Create(root(t, fs), []byte("append"), ModeRegular|0o644, 0, 0))
		block := make([]byte, 4096)
		for index := 0; index < count; index++ {
			block[0] = byte(index)
			ok(fs.WriteFile(file, uint64(index)*4096, block))
			after()
		}
	}},
	{"overwrite-4k", func(t testing.TB, fs *FS, count int, after func()) {
		file := ok(fs.Create(root(t, fs), []byte("overwrite"), ModeRegular|0o644, 0, 0))
		block := make([]byte, 4096)
		ok(fs.WriteFile(file, 0, make([]byte, 64*4096)))
		for index := 0; index < count; index++ {
			block[0] = byte(index)
			ok(fs.WriteFile(file, uint64(index%64)*4096, block))
			after()
		}
	}},
	{"create-unlink", func(t testing.TB, fs *FS, count int, after func()) {
		dir := root(t, fs)
		for index := 0; index < count; index++ {
			name := []byte(fmt.Sprintf("tmp-%05d", index))
			ok(fs.Create(dir, name, ModeRegular|0o644, 0, 0))
			if err := fs.Unlink(dir, name); err != nil {
				t.Fatal(err)
			}
			after()
		}
	}},
}

// TestOperationCost reports the device flushes and writes each workload
// costs per operation, once with a sync after every operation, as an
// application calling fsync each time would, and once batched. Run with -v
// to read them. A batch must stay below a tenth of a flush per operation.
func TestOperationCost(t *testing.T) {
	const count = 200
	for _, workload := range costWorkloads {
		for _, synced := range []bool{true, false} {
			fs, device := scratch(t, "canonical")
			after := func() {}
			if synced {
				after = func() {
					if err := fs.Sync(); err != nil {
						t.Fatal(err)
					}
				}
			}
			flushes, writes := device.flushes, device.writes
			workload.run(t, fs, count, after)
			if err := fs.Sync(); err != nil {
				t.Fatal(err)
			}
			perFlush := float64(device.flushes-flushes) / count
			mode := "batched"
			if synced {
				mode = "synced"
			}
			t.Logf("%-14s %-8s %5.2f flushes/op %6.2f writes/op", workload.name, mode, perFlush,
				float64(device.writes-writes)/count)
			if !synced && perFlush >= 0.1 {
				t.Errorf("%s: %.2f flushes per batched operation", workload.name, perFlush)
			}
			if err := fs.Unmount(); err != nil {
				t.Fatal(err)
			}
			checkVolume(t, device)
		}
	}
}

// TestFailedOperationKeepsTheBatch fails operations between successful
// ones in one batch: each failure drops only its own changes.
func TestFailedOperationKeepsTheBatch(t *testing.T) {
	fs, device := scratch(t, "canonical")
	dir := root(t, fs)
	ok(fs.Create(dir, []byte("first"), ModeRegular|0o644, 0, 0))
	file := ok(fs.Create(dir, []byte("filler"), ModeRegular|0o644, 0, 0))
	size := uint64(0)
	for fs.availableBlocks() > 200 {
		ok(fs.WriteFile(file, size, make([]byte, 1<<20)))
		size += 1 << 20
	}
	before := fs.Info()
	_, err := fs.Create(dir, []byte("first"), ModeRegular|0o644, 0, 0)
	wantKind(t, err, KindExists)
	// A write past the free space, small enough for one transaction,
	// stages bitmaps and descriptors the batch already holds, then fails.
	_, err = fs.WriteFile(file, size, make([]byte, (fs.availableBlocks()+16)*4096))
	wantKind(t, err, KindNoSpace)
	if after := fs.Info(); after.FreeBlocks != before.FreeBlocks || after.FreeInodes != before.FreeInodes {
		t.Fatalf("failed operations changed the counts: %d/%d, was %d/%d",
			after.FreeBlocks, after.FreeInodes, before.FreeBlocks, before.FreeInodes)
	}
	ok(fs.Create(dir, []byte("last"), ModeRegular|0o644, 0, 0))
	if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}
	checkVolume(t, device)
	reread := mount(t, device)
	for _, name := range []string{"first", "filler", "last"} {
		lookupInode(t, reread, root(t, reread), name)
	}
	if got := lookupInode(t, reread, root(t, reread), "filler"); got.Size != size {
		t.Fatalf("filler size %d after a failed write", got.Size)
	}
}
