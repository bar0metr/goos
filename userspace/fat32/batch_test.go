package fat32

import (
	"fmt"
	"goos/kernel/ramfs"
	"strings"
	"testing"
)

// batchNames lists a directory through ReadDirBatch and returns the names and
// the batches it took.
func batchNames(t *testing.T, fs *FileSystem, dir ramfs.NodeID) ([]string, []ramfs.DirRecord, int) {
	t.Helper()
	target := make([]byte, ramfs.MaxDirBatch)
	var names []string
	var records []ramfs.DirRecord
	for cursor, batches := 0, 1; ; batches++ {
		batch, status := fs.ReadDirBatch(dir, cursor, target)
		if status != ramfs.StatusOK {
			t.Fatalf("batch at %d: %v", cursor, status)
		}
		if !ramfs.ValidDirBatch(target[:batch.Used], batch.Count, cursor, batch.Next) {
			t.Fatalf("invalid batch at %d: %+v", cursor, batch)
		}
		for data := target[:batch.Used]; len(data) != 0; {
			var record ramfs.DirRecord
			size, _ := record.Decode(data)
			names = append(names, string(record.Name))
			record.Name = nil
			records = append(records, record)
			data = data[size:]
		}
		if batch.End {
			return names, records, batches
		}
		cursor = batch.Next
	}
}

// The batched scan and the single-entry scan agree on every name, identity,
// and listing position, in a directory larger than one batch.
func TestBatchMatchesSingleEntryScan(t *testing.T) {
	device := formatted(t, 64<<20, Layout{SectorsPerCluster: 8, FATs: 2})
	fs, root := mounted(t, device)
	dir := mkdir(t, fs, root, "many")
	name := func(i int) string {
		if i%3 == 0 {
			return fmt.Sprintf("S%05d.TXT", i)
		}
		return fmt.Sprintf("Файл номер %05d with a long name.txt", i)
	}
	const files = 300
	for i := 0; i < files; i++ {
		writeFile(t, fs, dir, name(i), fmt.Sprint(i))
	}
	for i := 0; i < files; i += 7 {
		if status := fs.UnlinkName(dir, mustName(t, name(i))); status != ramfs.StatusOK {
			t.Fatal(status)
		}
	}
	names, records, batches := batchNames(t, fs, dir)
	single := listNames(t, fs, dir)
	if strings.Join(names, "|") != strings.Join(single, "|") {
		t.Fatalf("batched %d names, single %d", len(names), len(single))
	}
	if batches < 2 {
		t.Fatalf("%d entries took %d batch", len(names), batches)
	}
	for cursor, index := 0, 0; ; index++ {
		entry, next, status := fs.ReadDir(dir, cursor)
		if status == ramfs.StatusNotFound {
			break
		}
		if status != ramfs.StatusOK || records[index].Stat != entry.Stat || records[index].Next != next {
			t.Fatalf("entry %d: batch %+v, single %+v next %d", index, records[index], entry.Stat, next)
		}
		cursor = next
	}
}

// A target too small for the next record stops at the start of its long chain,
// so a larger target reads the same entry whole.
func TestBatchStopsBeforeRecordThatDoesNotFit(t *testing.T) {
	device := formatted(t, 64<<20, Layout{SectorsPerCluster: 8, FATs: 2})
	fs, root := mounted(t, device)
	long := strings.Repeat("l", 200)
	writeFile(t, fs, root, "short.txt", "1")
	writeFile(t, fs, root, long, "2")
	target := make([]byte, ramfs.DirRecordSize(9)+ramfs.DirRecordSize(50))
	batch, status := fs.ReadDirBatch(root, 0, target)
	if status != ramfs.StatusOK || batch.Count != 1 || batch.End {
		t.Fatalf("first = %+v, %v", batch, status)
	}
	rest := make([]byte, ramfs.MaxDirBatch)
	second, status := fs.ReadDirBatch(root, batch.Next, rest)
	if status != ramfs.StatusOK || second.Count != 1 || !second.End {
		t.Fatalf("second = %+v, %v", second, status)
	}
	var record ramfs.DirRecord
	if _, ok := record.Decode(rest[:second.Used]); !ok || string(record.Name) != long {
		t.Fatalf("name = %q", record.Name)
	}
	empty, status := fs.ReadDirBatch(root, batch.Next, rest[:ramfs.DirRecordSize(1)])
	if status != ramfs.StatusOK || empty.Count != 0 || empty.End || empty.Next != batch.Next {
		t.Fatalf("no room = %+v, %v", empty, status)
	}
}

func TestBatchOfEmptyDirectoryAndBadParent(t *testing.T) {
	device := formatted(t, 64<<20, Layout{SectorsPerCluster: 8, FATs: 2})
	fs, root := mounted(t, device)
	dir := mkdir(t, fs, root, "empty")
	file := writeFile(t, fs, root, "file", "1")
	target := make([]byte, ramfs.MaxDirBatch)
	batch, status := fs.ReadDirBatch(dir, 0, target)
	if status != ramfs.StatusOK || batch.Count != 0 || !batch.End {
		t.Fatalf("empty = %+v, %v", batch, status)
	}
	_, batchStatus := fs.ReadDirBatch(file, 0, target)
	_, _, singleStatus := fs.ReadDir(file, 0)
	if batchStatus != singleStatus || batchStatus == ramfs.StatusOK {
		t.Fatalf("file as parent: batch %v, single %v", batchStatus, singleStatus)
	}
	if _, status := fs.ReadDirBatch(root, -1, target); status != ramfs.StatusInvalidArgument {
		t.Fatalf("negative cursor = %v", status)
	}
}
