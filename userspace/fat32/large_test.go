package fat32

import (
	"bytes"
	"fmt"
	"goos/kernel/ramfs"
	"os"
	"testing"
)

// fileDevice is a host file as a device, for volumes too large to hold in
// memory. The file is sparse until written.
type fileDevice struct {
	file *os.File
	size uint64
}

func (device fileDevice) ReadAt(offset uint64, target []byte) bool {
	_, err := device.file.ReadAt(target, int64(offset))
	return err == nil && offset+uint64(len(target)) <= device.size
}
func (device fileDevice) WriteAt(offset uint64, data []byte) bool {
	if offset+uint64(len(data)) > device.size {
		return false
	}
	_, err := device.file.WriteAt(data, int64(offset))
	return err == nil
}
func (fileDevice) Flush() bool             { return true }
func (device fileDevice) Capacity() uint64 { return device.size }

func TestLargeFiles(t *testing.T) {
	sizes := []int{1 << 20, 16 << 20, 64 << 20}
	for _, sector := range []uint16{512, 4096} {
		t.Run(fmt.Sprint(sector), func(t *testing.T) {
			// 4 KiB clusters either way.
			device := formatted(t, 100<<20, Layout{BytesPerSector: sector, SectorsPerCluster: uint8(4096 / sector), FATs: 2})
			fs, root := mounted(t, device)
			for i, size := range sizes {
				id, _ := fs.CreateName(root, mustName(t, fmt.Sprintf("large %d.bin", i)))
				writeChunks(t, fs, id, 0, pattern(size, byte(i+1)), 128<<10)
			}
			check(t, device)
			fs, root = mounted(t, device)
			for i, size := range sizes {
				id := mustOpen(t, fs, root, fmt.Sprintf("large %d.bin", i))
				if stat, _ := fs.Stat(id); stat.Size != uint64(size) {
					t.Fatalf("size %d, want %d", stat.Size, size)
				}
				if got := readAll(t, fs, id, size, 128<<10); !bytes.Equal(got, pattern(size, byte(i+1))) {
					t.Fatalf("%d-byte file differs", size)
				}
			}
			// Shrinking the largest frees exactly what it no longer needs.
			id := mustOpen(t, fs, root, "large 2.bin")
			before, _ := fs.FreeClusters()
			if status := fs.Truncate(id, 3<<20); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			after, _ := fs.FreeClusters()
			if want := uint32((64<<20 - 3<<20) / fs.clusterSize); after-before != want {
				t.Fatalf("truncate freed %d clusters, want %d", after-before, want)
			}
			check(t, device)
		})
	}
}

// TestFileSizeLimitIsFAT32s pins the only size limit: 4 GiB − 1, from the
// 32-bit size field. Anything past it is refused before any cluster is taken.
func TestFileSizeLimitIsFAT32s(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	id := writeFile(t, fs, root, "limit", "x")
	free, _ := fs.FreeClusters()
	if _, status := fs.Write(id, maxFileSize, []byte("y")); status != ramfs.StatusResourceExhausted {
		t.Fatalf("write past 4 GiB - 1: %v", status)
	}
	if _, status := fs.Write(id, maxFileSize-1, []byte("yz")); status != ramfs.StatusResourceExhausted {
		t.Fatalf("write across 4 GiB - 1: %v", status)
	}
	if status := fs.Truncate(id, maxFileSize+1); status != ramfs.StatusResourceExhausted {
		t.Fatalf("truncate past 4 GiB - 1: %v", status)
	}
	// Up to the limit it is only a matter of space.
	if status := fs.Truncate(id, maxFileSize); status != ramfs.StatusNoSpace {
		t.Fatalf("truncate to 4 GiB - 1 on a small volume: %v", status)
	}
	if after, _ := fs.FreeClusters(); after != free {
		t.Fatalf("refused requests changed the free count: %d -> %d", free, after)
	}
	check(t, device)
}

// TestVeryLargeFile writes a file of 1 GiB, and one of the FAT32 maximum,
// 4 GiB − 1, when there is space for it, through a sparse host file. It runs
// under make test-storage-stress.
func TestVeryLargeFile(t *testing.T) {
	if !stress() {
		t.Skip("GOOS_STORAGE_STRESS=1 runs it")
	}
	for _, size := range []uint64{1 << 30, maxFileSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			path := t.TempDir() + "/volume.img"
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			capacity := size + 64<<20
			if err := file.Truncate(int64(capacity)); err != nil {
				t.Skip(err)
			}
			device := fileDevice{file: file, size: capacity}
			if !Format(device, Layout{BytesPerSector: 4096, SectorsPerCluster: 8, FATs: 2}) {
				t.Fatal("format")
			}
			var fs FileSystem
			root, _ := fs.Mount(device)
			id, _ := fs.CreateName(root, mustName(t, "huge.bin"))
			// Growing writes zeros over the whole range; the ends then get
			// data.
			if status := fs.Truncate(id, size); status != ramfs.StatusOK {
				if status == ramfs.StatusIOError {
					t.Skip("host ran out of space")
				}
				t.Fatal(status)
			}
			head, tail := pattern(1<<20, 1), pattern(1<<20, 2)
			fs.Write(id, 0, head)
			fs.Write(id, size-uint64(len(tail)), tail)
			var again FileSystem
			root, _ = again.Mount(device)
			id = mustOpen(t, &again, root, "huge.bin")
			if stat, _ := again.Stat(id); stat.Size != size {
				t.Fatalf("size %d", stat.Size)
			}
			got := make([]byte, len(tail))
			again.Read(id, size-uint64(len(tail)), got)
			if !bytes.Equal(got, tail) {
				t.Fatal("tail differs")
			}
			again.Read(id, size/2, got)
			if !bytes.Equal(got, make([]byte, len(got))) {
				t.Fatal("the middle is not zero")
			}
			if status := again.UnlinkName(root, mustName(t, "huge.bin")); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			if free, _ := again.FreeClusters(); free != again.clusters-1 {
				t.Fatalf("%d of %d clusters free after removal", free, again.clusters)
			}
		})
	}
}

// TestFragmentedFiles interleaves appends to several files so that their
// chains alternate, punches holes by removing some, and fills the holes with
// a new file whose chain then jumps across the volume.
func TestFragmentedFiles(t *testing.T) {
	device := formatted(t, 16<<20, Layout{BytesPerSector: 1024, SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	const files, rounds = 5, 300
	ids := make([]ramfs.NodeID, files)
	contents := make([][]byte, files)
	for i := range ids {
		ids[i] = writeFile(t, fs, root, fmt.Sprintf("fragment %d", i), "")
	}
	for round := 0; round < rounds; round++ {
		for i, id := range ids {
			chunk := pattern(1024+round%3*7, byte(round*files+i))
			if _, status := fs.Write(id, uint64(len(contents[i])), chunk); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			contents[i] = append(contents[i], chunk...)
		}
	}
	// The chains really do alternate.
	first := entryClusterOf(t, fs, ids[0])
	if next, _, _ := fs.next(first); next == first+1 {
		t.Fatal("the files were not fragmented")
	}
	for _, i := range []int{1, 3} {
		fs.UnlinkName(root, mustName(t, fmt.Sprintf("fragment %d", i)))
	}
	filler := make([]byte, 2*rounds*1024)
	for i := range filler {
		filler[i] = byte(i * 13)
	}
	id, _ := fs.CreateName(root, mustName(t, "filler"))
	fs.hint = 2 // start the search in the holes
	writeChunks(t, fs, id, 0, filler, 50000)
	fs, root = mounted(t, device)
	for _, i := range []int{0, 2, 4} {
		got := readAll(t, fs, mustOpen(t, fs, root, fmt.Sprintf("fragment %d", i)), len(contents[i]), 3000)
		if !bytes.Equal(got, contents[i]) {
			t.Fatalf("fragment %d differs", i)
		}
	}
	if got := readAll(t, fs, mustOpen(t, fs, root, "filler"), len(filler), 7777); !bytes.Equal(got, filler) {
		t.Fatal("filler differs")
	}
	check(t, device)
}

// TestLargeDirectoryOfLongNames fills one directory with thousands of long,
// partly non-ASCII names, removes every third, refills the holes, and looks
// every name up after a remount.
func TestLargeDirectoryOfLongNames(t *testing.T) {
	files := 1500
	if stress() {
		// Four entries each: 32000 of the 65536 a directory may hold.
		files = 8000
	}
	device := formatted(t, 64<<20, Layout{SectorsPerCluster: 8, FATs: 2})
	fs, root := mounted(t, device)
	dir := mkdir(t, fs, root, "many")
	name := func(i int) string { return fmt.Sprintf("Файл номер %05d with a long name.txt", i) }
	for i := 0; i < files; i++ {
		writeFile(t, fs, dir, name(i), fmt.Sprint(i))
	}
	for i := 0; i < files; i += 3 {
		if status := fs.UnlinkName(dir, mustName(t, name(i))); status != ramfs.StatusOK {
			t.Fatal(status)
		}
	}
	for i := 0; i < files; i += 3 {
		writeFile(t, fs, dir, name(i), fmt.Sprint(i))
	}
	parsed, _ := fs.directoryOf(dir)
	if entries := fs.entries(&parsed); entries < files*4 {
		t.Fatalf("%d entries for %d four-entry names", entries, files)
	}
	fs, root = mounted(t, device)
	dir = mustOpen(t, fs, root, "many")
	if names := listNames(t, fs, dir); len(names) != files {
		t.Fatalf("listed %d of %d", len(names), files)
	}
	for i := 0; i < files; i += 97 {
		if got := readFile(t, fs, mustOpen(t, fs, dir, name(i))); got != fmt.Sprint(i) {
			t.Fatalf("%s read %q", name(i), got)
		}
	}
	check(t, device)
}
