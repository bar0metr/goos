package ext4vol

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"sort"
	"testing"

	"goos/kernel/ramfs"
)

// manyFixture loads the 10000-name indexed fixture the prefetch regression
// tests use (userspace/ext4/testdata/many.img.gz).
func manyFixture(t *testing.T) *memDevice {
	t.Helper()
	compressed, err := os.ReadFile("../ext4/testdata/many.img.gz")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	image, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return &memDevice{data: image}
}

func mountedMany(t *testing.T) *Volume {
	t.Helper()
	var volume Volume
	if !volume.Mount(manyFixture(t)) {
		t.Fatalf("mount refused: %s", volume.Refusal())
	}
	return &volume
}

// batchNames pages dir with the given budget and returns the names in
// listing order.
func batchNames(t *testing.T, volume *Volume, dir ramfs.NodeID, budget, start int) []string {
	t.Helper()
	var names []string
	cursor := start
	for rounds := 0; ; rounds++ {
		if rounds > 10000 {
			t.Fatal("listing does not end")
		}
		target := make([]byte, budget)
		batch, status := volume.ReadDirBatch(dir, cursor, target)
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		if !ramfs.ValidDirBatch(target[:batch.Used], batch.Count, cursor, batch.Next) {
			t.Fatal("malformed batch")
		}
		var record ramfs.DirRecord
		for data := target[:batch.Used]; len(data) > 0; {
			size, _ := record.Decode(data)
			names = append(names, string(record.Name))
			data = data[size:]
		}
		cursor = batch.Next
		if batch.End {
			break
		}
	}
	return names
}

// TestReadDirBatchManyDirectory pages the 10000-name indexed directory with
// two byte budgets and requires the same complete listing (ADR 0231).
func TestReadDirBatchManyDirectory(t *testing.T) {
	volume := mountedMany(t)
	root, _ := volume.Root()
	many, status := volume.OpenName(root, name(t, "many"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	small := batchNames(t, volume, many, 200, 0)
	large := batchNames(t, volume, many, ramfs.MaxDirBatch, 0)
	if len(small) != 10000 || len(large) != 10000 {
		t.Fatalf("paged listings saw %d and %d names, want 10000", len(small), len(large))
	}
	for index := 0; index < 10000; index++ {
		if small[index] != large[index] {
			t.Fatalf("listing order differs at %d: %q vs %q", index, small[index], large[index])
		}
	}
	sorted := append([]string(nil), large...)
	sort.Strings(sorted)
	for index := 0; index < 10000; index++ {
		want := []byte{'f', byte('0' + index/10000), byte('0' + index/1000%10), byte('0' + index/100%10), byte('0' + index/10%10), byte('0' + index%10)}
		if sorted[index] != string(want) {
			t.Fatalf("name %d: %q, want %q", index, sorted[index], want)
		}
	}
}

// TestReadDirBatchInterleavedStreams interleaves two listings of the same
// directory, the second starting where the first's initial batch ended, and
// requires each stream to stay whole: the prefetch state must not leak
// entries or cursors between streams (ADR 0231).
func TestReadDirBatchInterleavedStreams(t *testing.T) {
	volume := mountedMany(t)
	root, _ := volume.Root()
	many, _ := volume.OpenName(root, name(t, "many"))

	target := make([]byte, 251)
	first, status := volume.ReadDirBatch(many, 0, target)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	prefix := batchNames(t, volume, many, 251, 0)
	suffix := batchNames(t, volume, many, 251, first.Next)
	if len(suffix)+first.Count != len(prefix) {
		t.Fatalf("middle stream %d + %d != whole %d", len(suffix), first.Count, len(prefix))
	}
	if len(prefix) != 10000 || len(suffix) != 10000-first.Count {
		t.Fatalf("streams saw %d and %d names", len(prefix), len(suffix))
	}
	for index, name := range suffix {
		if name != prefix[first.Count+index] {
			t.Fatalf("suffix entry %d: %q, want %q", index, name, prefix[first.Count+index])
		}
	}
}

// TestReadDirBatchMatchesSingleEntry lists the links fixture both ways and
// requires identical stats per name: hard-link identity, FIFO and device
// kinds and device numbers survive the prefetch (ADR 0231).
func TestReadDirBatchMatchesSingleEntry(t *testing.T) {
	compressed, err := os.ReadFile("../ext4/testdata/links.img.gz")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	image, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var volume Volume
	if !volume.Mount(&memDevice{data: image}) {
		t.Fatalf("mount refused: %s", volume.Refusal())
	}
	root, _ := volume.Root()
	single := map[string]ramfs.Stat{}
	for cursor := 0; ; {
		entry, next, status := volume.ReadDir(root, cursor)
		if status == ramfs.StatusNotFound {
			break
		}
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		single[string(entry.Name[:entry.Size])] = entry.Stat
		cursor = next
	}
	// Batch pages and re-reads each name's stat through the single path.
	for _, listed := range batchNames(t, &volume, root, ramfs.MaxDirBatch, 0) {
		node, status := volume.OpenName(root, name(t, listed))
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		stat, status := volume.Stat(node)
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		if stat != single[listed] {
			t.Fatalf("%s: batch stat %+v != single stat %+v", listed, stat, single[listed])
		}
	}
}
