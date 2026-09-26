package fat32

import (
	"fmt"
	"goos/kernel/ramfs"
	"os"
	"testing"
)

// sectorSizes are the logical sector sizes FAT32 allows.
var sectorSizes = []uint16{512, 1024, 2048, 4096}

// exercise runs a workload that touches every derived offset: nested
// directories that grow past one cluster, files that cross cluster and sector
// boundaries at odd offsets, truncation both ways, a cross-directory move, and
// removal. It then remounts, reads everything back and checks the volume.
func exercise(t *testing.T, device Device) {
	t.Helper()
	fs, root := mounted(t, device)
	outer := mkdir(t, fs, root, "outer")
	inner := mkdir(t, fs, outer, "Inner Directory")
	cluster := int(fs.clusterSize)
	sizes := []int{0, 1, int(fs.sectorSize) - 1, int(fs.sectorSize) + 1, cluster - 3, cluster + 5, 3*cluster + 7}
	for i, size := range sizes {
		id, status := fs.CreateName(inner, mustName(t, fmt.Sprintf("File %02d.bin", i)))
		if status != ramfs.StatusOK {
			t.Fatalf("create %d: %v", i, status)
		}
		writeChunks(t, fs, id, 0, pattern(size, byte(i)), 997)
	}
	// Enough entries to need more than one directory cluster whatever the
	// cluster size, up to 64 KiB clusters.
	entries := int(fs.clusterSize/entrySize) + 20
	if entries > 2100 {
		entries = 2100
	}
	for i := 0; i < entries; i++ {
		writeFile(t, fs, outer, fmt.Sprintf("n%04d", i), "")
	}
	odd, _ := fs.CreateName(root, mustName(t, "odd offsets.dat"))
	if count, status := fs.Write(odd, uint64(cluster)+13, []byte("past a hole")); status != ramfs.StatusOK || count != 11 {
		t.Fatalf("write past a hole: %d %v", count, status)
	}
	if status := fs.Truncate(odd, uint64(2*cluster+1)); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := fs.Truncate(odd, uint64(cluster)+20); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	moved := mkdir(t, fs, root, "moved")
	if status := fs.RenameName(inner, moved, mustName(t, "inner")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := fs.UnlinkName(outer, mustName(t, "n0003")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	check(t, device)

	fs, root = mounted(t, device)
	inner = mustOpen(t, fs, mustOpen(t, fs, root, "moved"), "INNER")
	for i, size := range sizes {
		id := mustOpen(t, fs, inner, fmt.Sprintf("file %02d.BIN", i))
		if got := readAll(t, fs, id, size, 1009); string(got) != string(pattern(size, byte(i))) {
			t.Fatalf("file %d of %d bytes read back wrong", i, size)
		}
	}
	got := readAll(t, fs, mustOpen(t, fs, root, "odd offsets.dat"), cluster+20, 4096)
	for i, value := range got {
		want := byte(0)
		if i >= cluster+13 {
			want = "past a hole"[i-cluster-13]
		}
		if value != want {
			t.Fatalf("odd offsets byte %d = %#x, want %#x", i, value, want)
		}
	}
	if names := listNames(t, fs, mustOpen(t, fs, root, "outer")); len(names) != entries-1 {
		t.Fatalf("outer lists %d entries, want %d", len(names), entries-1)
	}
}

func TestEverySectorSizeAndClusterSize(t *testing.T) {
	for _, sector := range sectorSizes {
		for perCluster := uint8(1); perCluster != 0; perCluster <<= 1 {
			size := uint64(sector) * uint64(perCluster)
			if size > 64<<10 && !stress() {
				// 128 KiB to 512 KiB clusters are valid, and covered by the
				// stress run; they make each case large.
				continue
			}
			for _, fats := range []uint8{1, 2} {
				layout := Layout{BytesPerSector: sector, SectorsPerCluster: perCluster, FATs: fats}
				t.Run(fmt.Sprintf("%dB-sectors-%dx-%dfat", sector, perCluster, fats), func(t *testing.T) {
					// Room for a few hundred clusters of any size.
					capacity := max(uint64(8<<20), size*400)
					device := formatted(t, capacity, layout)
					var fs FileSystem
					if _, ok := fs.Mount(device); !ok || fs.sectorSize != uint64(sector) || fs.clusterSize != size {
						t.Fatalf("mounted %v: sector %d cluster %d", ok, fs.sectorSize, fs.clusterSize)
					}
					exercise(t, device)
				})
			}
		}
	}
}

func TestUnmirroredActiveFATWithEverySectorSize(t *testing.T) {
	for _, sector := range sectorSizes {
		t.Run(fmt.Sprint(sector), func(t *testing.T) {
			device := formatted(t, 16<<20, Layout{BytesPerSector: sector, SectorsPerCluster: 2, FATs: 2, NoMirror: true, ActiveFAT: 1})
			var fs FileSystem
			fs.Mount(device)
			firstFAT := string(device.data[fs.fatStart : fs.fatStart+fs.fatSize])
			exercise(t, device)
			if string(device.data[fs.fatStart:fs.fatStart+fs.fatSize]) != firstFAT {
				t.Fatal("the inactive FAT was written")
			}
			if _, status := fs.CompareFATs(); status != ramfs.StatusUnsupported {
				t.Fatalf("comparing unmirrored copies: %v", status)
			}
		})
	}
}

// TestLargeSectorVolumeDoesNotAssume512 pins the offsets on a 4 KiB-sector
// volume: the FAT starts after 32 reserved 4 KiB sectors and cluster 2 after
// the FATs, all in 4 KiB units.
func TestLargeSectorVolumeDoesNotAssume512(t *testing.T) {
	device := formatted(t, 32<<20, Layout{BytesPerSector: 4096, SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	fatSectors := uint64(get32(device.data[bpbFATSize32:]))
	if fs.fatStart != 32*4096 || fs.fatSize != fatSectors*4096 || fs.dataStart != (32+2*fatSectors)*4096 {
		t.Fatalf("fat at %d size %d, data at %d", fs.fatStart, fs.fatSize, fs.dataStart)
	}
	id := writeFile(t, fs, root, "where.txt", "here")
	cluster := entryClusterOf(t, fs, id)
	offset := fs.dataStart + uint64(cluster-2)*4096
	if string(device.data[offset:offset+4]) != "here" {
		t.Fatal("data is not where 4 KiB sectors put it")
	}
	// The FSInfo sector is logical sector 1: byte 4096, not 512.
	if get32(device.data[4096:]) != fsInfoLead || fs.fsInfo != 4096 {
		t.Fatalf("FSInfo at %d", fs.fsInfo)
	}
	if get32(device.data[4096+fsInfoNextFree:]) != fs.hint {
		t.Fatal("the FSInfo hint was written somewhere else")
	}
}

func TestFormatRefusesInvalidLayouts(t *testing.T) {
	for _, layout := range []Layout{
		{BytesPerSector: 256, SectorsPerCluster: 1, FATs: 2},
		{BytesPerSector: 8192, SectorsPerCluster: 1, FATs: 2},
		{BytesPerSector: 1000, SectorsPerCluster: 1, FATs: 2},
		{SectorsPerCluster: 3, FATs: 2},
		{SectorsPerCluster: 0, FATs: 2},
		{SectorsPerCluster: 1, FATs: 0},
		{SectorsPerCluster: 1, FATs: 3},
		{SectorsPerCluster: 1, FATs: 2, NoMirror: true, ActiveFAT: 2},
	} {
		if Format(&memoryDevice{data: make([]byte, 8<<20)}, layout) {
			t.Errorf("formatted %+v", layout)
		}
	}
	if Format(&memoryDevice{data: make([]byte, 16<<10)}, Layout{SectorsPerCluster: 1, FATs: 2}) {
		t.Error("formatted a device too small for its own metadata")
	}
}

// stress reports whether the larger storage cases should run
// (make test-storage-stress).
func stress() bool { return os.Getenv("GOOS_STORAGE_STRESS") == "1" }
