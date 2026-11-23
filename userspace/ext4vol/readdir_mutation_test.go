package ext4vol

import (
	"testing"

	"goos/kernel/ramfs"
)

// collectedEntry is one name and its listed size.
type collectedEntry struct {
	name string
	size uint64
}

// collectEntries pages dir from start and returns each record's name and
// size.
func collectEntries(t *testing.T, volume *Volume, dir ramfs.NodeID, budget, start int) []collectedEntry {
	t.Helper()
	var result []collectedEntry
	cursor := start
	for rounds := 0; ; rounds++ {
		if rounds > 20000 {
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
			result = append(result, collectedEntry{name: string(record.Name), size: record.Stat.Size})
			data = data[size:]
		}
		cursor = batch.Next
		if batch.End {
			return result
		}
	}
}

// bigReference returns the ordered names of /big on a read-only mount.
func bigReference(t *testing.T) []string {
	t.Helper()
	volume := mounted(t)
	root, _ := volume.Root()
	big, _ := volume.OpenName(root, name(t, "big"))
	return batchNames(t, volume, big, ramfs.MaxDirBatch, 0)
}

// TestReadDirBatchInvalidatedByUnlink unlinks an entry that is in the decoded
// batch but not yet returned, then continues the listing: the stale decoded
// entry must not come back (ADR 0231).
func TestReadDirBatchInvalidatedByUnlink(t *testing.T) {
	reference := bigReference(t)
	volume, _ := writable(t)
	root, _ := volume.Root()
	big, _ := volume.OpenName(root, name(t, "big"))

	target := make([]byte, 200)
	batch, status := volume.ReadDirBatch(big, 0, target)
	must(t, status)
	if batch.End || batch.Count == 0 || batch.Count >= len(reference) {
		t.Fatal("expected a partial batch")
	}
	unlinked := reference[batch.Count]
	must(t, volume.UnlinkName(big, name(t, unlinked)))

	for _, entry := range collectEntries(t, volume, big, ramfs.MaxDirBatch, batch.Next) {
		if entry.name == unlinked {
			t.Fatalf("unlinked %q still listed", unlinked)
		}
	}
}

// TestReadDirBatchInvalidatedByMetadataMutation truncates a not-yet-returned
// file and requires the continuation to report the new size, not the stale
// decoded one (ADR 0231).
func TestReadDirBatchInvalidatedByMetadataMutation(t *testing.T) {
	reference := bigReference(t)
	volume, _ := writable(t)
	root, _ := volume.Root()
	big, _ := volume.OpenName(root, name(t, "big"))

	target := make([]byte, 200)
	batch, status := volume.ReadDirBatch(big, 0, target)
	must(t, status)
	if batch.End || batch.Count == 0 || batch.Count >= len(reference) {
		t.Fatal("expected a partial batch")
	}
	changed := reference[batch.Count]
	file, status := volume.OpenName(big, name(t, changed))
	must(t, status)
	must(t, volume.Truncate(file, 7777))

	found := false
	for _, entry := range collectEntries(t, volume, big, ramfs.MaxDirBatch, batch.Next) {
		if entry.name == changed {
			found = true
			if entry.size != 7777 {
				t.Fatalf("%q listed with size %d, want 7777", changed, entry.size)
			}
		}
	}
	if !found {
		t.Fatalf("%q not listed after truncate", changed)
	}
}

// TestReadDirBatchInvalidatedByCreate creates a file mid-listing and requires
// a consistent continuation: the new name appears once and nothing repeats
// (ADR 0231).
func TestReadDirBatchInvalidatedByCreate(t *testing.T) {
	volume, _ := writable(t)
	root, _ := volume.Root()
	big, _ := volume.OpenName(root, name(t, "big"))

	target := make([]byte, 200)
	batch, status := volume.ReadDirBatch(big, 0, target)
	must(t, status)
	if batch.End || batch.Count == 0 {
		t.Fatal("expected a partial batch")
	}
	_, status = volume.CreateName(big, name(t, "zzz-new"))
	must(t, status)

	seen := map[string]bool{}
	found := false
	for _, entry := range collectEntries(t, volume, big, ramfs.MaxDirBatch, batch.Next) {
		if seen[entry.name] {
			t.Fatalf("duplicate entry %q", entry.name)
		}
		seen[entry.name] = true
		if entry.name == "zzz-new" {
			found = true
			if entry.size != 0 {
				t.Fatalf("zzz-new listed with size %d, want 0", entry.size)
			}
		}
	}
	if !found {
		t.Fatal("zzz-new not listed after create")
	}
}

// TestReadDirBatchInvalidatedByPartialWrite fills a not-yet-returned file
// until the volume is full, so the last write commits a prefix and the next
// fails with no space. The continuation must refill and report the new size,
// not the stale decoded one (ADR 0231).
func TestReadDirBatchInvalidatedByPartialWrite(t *testing.T) {
	reference := bigReference(t)
	volume, _ := writable(t)
	root, _ := volume.Root()
	big, _ := volume.OpenName(root, name(t, "big"))

	target := make([]byte, 200)
	batch, status := volume.ReadDirBatch(big, 0, target)
	must(t, status)
	if batch.End || batch.Count == 0 || batch.Count >= len(reference) {
		t.Fatal("expected a partial batch")
	}
	nextName := reference[batch.Count]
	file, status := volume.OpenName(big, name(t, nextName))
	must(t, status)

	chunk := make([]byte, 4<<20)
	var size uint64
	for {
		n, status := volume.Write(file, size, chunk)
		if status == ramfs.StatusNoSpace {
			break
		}
		must(t, status)
		size += uint64(n)
	}

	found := false
	for _, entry := range collectEntries(t, volume, big, ramfs.MaxDirBatch, batch.Next) {
		if entry.name == nextName {
			found = true
			if entry.size != size {
				t.Fatalf("%q listed with size %d, want %d", nextName, entry.size, size)
			}
		}
	}
	if !found {
		t.Fatalf("%q not listed after partial write", nextName)
	}
}
