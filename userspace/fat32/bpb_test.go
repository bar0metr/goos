package fat32

import (
	"fmt"
	"goos/kernel/ramfs"
	"testing"
)

func TestBackupBootSectorIsCompared(t *testing.T) {
	for _, sector := range sectorSizes {
		t.Run(fmt.Sprint(sector), func(t *testing.T) {
			device := formatted(t, 16<<20, Layout{BytesPerSector: sector, SectorsPerCluster: 1, FATs: 2, VolumeID: 0x1234})
			backup := device.data[6*uint64(sector):]
			fs, _ := mounted(t, device)
			if health := fs.Health(); health.Backup != BackupMatches || health.Degraded() {
				t.Fatalf("fresh volume: %+v", health)
			}
			// A differing BPB field in the backup is reported; the primary
			// still governs.
			put32(backup[67:71], 0x9999)
			fs, _ = mounted(t, device)
			if health := fs.Health(); health.Backup != BackupDiffers || !health.Degraded() || health.ReadOnly {
				t.Fatalf("differing backup: %+v", health)
			}
			// A backup that is not a boot sector at all is invalid.
			backup[510] = 0
			fs, _ = mounted(t, device)
			if health := fs.Health(); health.Backup != BackupInvalid {
				t.Fatalf("broken backup: %+v", health)
			}
			// A backup outside the reserved region is never read.
			put16(device.data[bpbBackup:], 40)
			fs, _ = mounted(t, device)
			if health := fs.Health(); health.Backup != BackupInvalid {
				t.Fatalf("backup past the reserved region: %+v", health)
			}
			put16(device.data[bpbBackup:], 0)
			fs, _ = mounted(t, device)
			if health := fs.Health(); health.Backup != BackupAbsent || health.Degraded() {
				t.Fatalf("no backup: %+v", health)
			}
		})
	}
}

// TestCorruptPrimaryMountsFromBackupReadOnly covers recovery: when the primary
// boot sector fails validation the backup at sector 6 is used, for any sector
// size, and the volume is served read-only with nothing rewritten.
func TestCorruptPrimaryMountsFromBackupReadOnly(t *testing.T) {
	for _, sector := range sectorSizes {
		t.Run(fmt.Sprint(sector), func(t *testing.T) {
			device := formatted(t, 16<<20, Layout{BytesPerSector: sector, SectorsPerCluster: 2, FATs: 2})
			fs, root := mounted(t, device)
			dir := mkdir(t, fs, root, "Документы")
			writeFile(t, fs, dir, "kept.txt", "survives")
			// Damage the primary geometry: bytes per sector of 1000.
			put16(device.data[bpbBytes:], 1000)
			damaged := string(device.data[:512])
			var recovered FileSystem
			root, ok := recovered.Mount(device)
			if !ok {
				t.Fatalf("not mounted from the backup: %v", recovered.Refused())
			}
			health := recovered.Health()
			if health.Boot != BootBackup || !health.ReadOnly || !health.Degraded() {
				t.Fatalf("health %+v", health)
			}
			if got := readFile(t, &recovered, mustOpen(t, &recovered, mustOpen(t, &recovered, root, "документы"), "kept.txt")); got != "survives" {
				t.Fatalf("read %q", got)
			}
			if _, status := recovered.CreateName(root, mustName(t, "new")); status != ramfs.StatusPermissionDenied {
				t.Fatalf("create on a recovered volume: %v", status)
			}
			id := mustOpen(t, &recovered, mustOpen(t, &recovered, root, "Документы"), "kept.txt")
			if _, status := recovered.Write(id, 0, []byte("x")); status != ramfs.StatusPermissionDenied {
				t.Fatalf("write on a recovered volume: %v", status)
			}
			for name, status := range map[string]ramfs.Status{
				"truncate": recovered.Truncate(id, 0),
				"unlink":   recovered.UnlinkName(root, mustName(t, "Документы")),
				"rename":   recovered.RenameName(id, root, mustName(t, "moved")),
			} {
				if status != ramfs.StatusPermissionDenied {
					t.Fatalf("%s on a recovered volume: %v", name, status)
				}
			}
			if _, status := recovered.MkdirName(root, mustName(t, "d")); status != ramfs.StatusPermissionDenied {
				t.Fatalf("mkdir on a recovered volume: %v", status)
			}
			if !recovered.Flush() {
				t.Fatal("flush of a read-only volume failed")
			}
			if string(device.data[:512]) != damaged {
				t.Fatal("mounting rewrote the damaged primary")
			}
		})
	}
}

func TestNoUsableBackupLeavesCorruptVolumeRefused(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	device.data[510] = 0
	device.data[6*512+510] = 0
	var fs FileSystem
	if _, ok := fs.Mount(device); ok || fs.Refused() != ramfs.StatusCorrupt {
		t.Fatalf("mounted %v, refused %v", ok, fs.Refused())
	}
	// A primary that is unsupported rather than corrupt is not second-guessed
	// from the backup: that is a different filesystem, not a damaged one.
	device = formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	put16(device.data[bpbRootEntries:], 512)
	if _, ok := fs.Mount(device); ok || fs.Refused() != ramfs.StatusUnsupported {
		t.Fatalf("FAT16-looking primary: mounted %v, refused %v", ok, fs.Refused())
	}
	// A backup that does not name itself as the backup is not trusted.
	device = formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	device.data[510] = 0
	put16(device.data[6*512+bpbBackup:], 0)
	if _, ok := fs.Mount(device); ok {
		t.Fatal("a backup that does not name itself was used")
	}
}

func TestFSInfoLocationAndSignatures(t *testing.T) {
	for name, damage := range map[string]func(data []byte){
		"lead signature":   func(data []byte) { data[512] = 0 },
		"struct signature": func(data []byte) { data[512+fsInfoStructAt] = 0 },
		"trail signature":  func(data []byte) { data[512+fsInfoTrailAt+3] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
			damage(device.data)
			fs, root := mounted(t, device)
			if health := fs.Health(); health.FSInfo != FSInfoInvalid || health.BackupFSInfo != BackupFSInfoMatches || !health.Degraded() || fs.fsInfo != 7*512 {
				t.Fatalf("health %+v", health)
			}
			writeFile(t, fs, root, "file", "data")
			primary, primaryOK := fs.readFSInfoAt(512)
			backup, backupOK := fs.readFSInfoAt(7 * 512)
			if !primaryOK || !backupOK || primary != backup {
				t.Fatalf("clean sync did not reconcile FSInfo: %+v/%v %+v/%v", primary, primaryOK, backup, backupOK)
			}
		})
	}
	for name, damage := range map[string]func(data []byte){
		"past the reserved": func(data []byte) { put16(data[bpbFSInfo:], 32) },
		"on the backup":     func(data []byte) { put16(data[bpbFSInfo:], 6) },
	} {
		t.Run(name, func(t *testing.T) {
			device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
			damage(device.data)
			before := string(device.data[:32*512])
			fs, root := mounted(t, device)
			if health := fs.Health(); health.FSInfo != FSInfoInvalid || !health.Degraded() || fs.fsInfo != 0 {
				t.Fatalf("health %+v", health)
			}
			writeFile(t, fs, root, "file", "data")
			if after := string(device.data[:32*512]); after != before {
				t.Fatal("an unaddressable FSInfo sector was written")
			}
		})
	}
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	put16(device.data[bpbFSInfo:], 0xffff)
	if fs, _ := mounted(t, device); fs.Health().FSInfo != FSInfoAbsent {
		t.Fatal("0xffff is 'no FSInfo'")
	}
}

func TestBackupFSInfoMismatchIsReportedAndCleanSyncReconciles(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	put32(device.data[7*512+fsInfoFreeCount:], 17)
	put32(device.data[7*512+fsInfoNextFree:], 23)
	fs, _ := mounted(t, device)
	if health := fs.Health(); health.BackupFSInfo != BackupFSInfoDiffers || !health.Degraded() {
		t.Fatalf("mismatch not reported: %+v", health)
	}
	if !fs.Flush() {
		t.Fatal("clean sync")
	}
	primary, primaryOK := fs.readFSInfoAt(512)
	backup, backupOK := fs.readFSInfoAt(7 * 512)
	if !primaryOK || !backupOK || primary != backup {
		t.Fatalf("copies differ after sync: %+v/%v %+v/%v", primary, primaryOK, backup, backupOK)
	}
}

func TestInvalidBackupFSInfoFallsBackToPrimary(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	device.data[7*512] = 0
	fs, _ := mounted(t, device)
	if health := fs.Health(); health.FSInfo != FSInfoValid || health.BackupFSInfo != BackupFSInfoInvalid || fs.fsInfo != 512 {
		t.Fatalf("primary fallback not reported: %+v", health)
	}
	if !fs.Flush() {
		t.Fatal("clean sync")
	}
	if _, ok := fs.readFSInfoAt(7 * 512); !ok {
		t.Fatal("clean sync did not restore backup FSInfo")
	}
}

// TestFSInfoFreeCountIsMaintained covers the free count: never taken from
// the sector, counted from the FAT the first time a change needs it, kept
// exact through allocation and release, and written with every sync.
func TestFSInfoFreeCountIsMaintained(t *testing.T) {
	for _, sector := range []uint16{512, 4096} {
		t.Run(fmt.Sprint(sector), func(t *testing.T) {
			device := formatted(t, 16<<20, Layout{BytesPerSector: sector, SectorsPerCluster: 1, FATs: 2})
			info := device.data[sector:]
			put32(info[fsInfoFreeCount:], 7) // a lie
			fs, root := mounted(t, device)
			if fs.recordedFree != 7 || fs.freeKnown {
				t.Fatal("the recorded count was believed")
			}
			recount := func() uint32 {
				t.Helper()
				var fresh FileSystem
				fresh.Mount(device)
				free, status := fresh.FreeClusters()
				if status != ramfs.StatusOK {
					t.Fatal(status)
				}
				return free
			}
			dir := mkdir(t, fs, root, "dir")
			big, _ := fs.CreateName(dir, mustName(t, "big"))
			writeChunks(t, fs, big, 0, pattern(300000, 3), 65536)
			if got, want := get32(info[fsInfoFreeCount:]), recount(); got != want {
				t.Fatalf("after writes FSInfo says %d free, the FAT %d", got, want)
			}
			fs.Truncate(big, 1000)
			fs.UnlinkName(root, mustName(t, "nothing"))
			if got, want := get32(info[fsInfoFreeCount:]), recount(); got != want {
				t.Fatalf("after truncation FSInfo says %d free, the FAT %d", got, want)
			}
			fs.UnlinkName(dir, mustName(t, "big"))
			fs.UnlinkName(root, mustName(t, "dir"))
			if got := get32(info[fsInfoFreeCount:]); got != fs.clusters-1 {
				t.Fatalf("empty volume: FSInfo says %d free of %d", got, fs.clusters)
			}
			check(t, device)
		})
	}
}

func TestFreeCountReachesZeroWhenFull(t *testing.T) {
	device := formatted(t, 2<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	id, _ := fs.CreateName(root, mustName(t, "fill"))
	chunk := make([]byte, 512)
	for offset := uint64(0); ; offset += 512 {
		if _, status := fs.Write(id, offset, chunk); status != ramfs.StatusOK {
			if status != ramfs.StatusNoSpace {
				t.Fatal(status)
			}
			break
		}
	}
	if free, _ := fs.FreeClusters(); free != 0 || get32(device.data[512+fsInfoFreeCount:]) != 0 {
		t.Fatalf("full volume: %d free, FSInfo %d", free, get32(device.data[512+fsInfoFreeCount:]))
	}
	check(t, device)
}

// TestFATMirrorMismatchIsDetectedAndHealed covers mirrored FATs: a copy that
// differs is found by CompareFATs, the first copy governs reads, and the next
// change in a differing unit writes the first copy's unit over the others.
func TestFATMirrorMismatchIsDetectedAndHealed(t *testing.T) {
	for _, sector := range []uint16{512, 2048} {
		t.Run(fmt.Sprint(sector), func(t *testing.T) {
			device := formatted(t, 16<<20, Layout{BytesPerSector: sector, SectorsPerCluster: 1, FATs: 2})
			fs, root := mounted(t, device)
			id := writeFile(t, fs, root, "file", string(pattern(5000, 1)))
			if mismatch, status := fs.CompareFATs(); status != ramfs.StatusOK || mismatch.Entries != 0 {
				t.Fatalf("fresh copies differ: %+v %v", mismatch, status)
			}
			// Point the second copy's entry for the file's first cluster at
			// a free cluster.
			first := entryClusterOf(t, fs, id)
			second := fs.fatStart + fs.fatSize + uint64(first)*4
			put32(device.data[second:], 3000)
			fs, root = mounted(t, device)
			mismatch, status := fs.CompareFATs()
			if status != ramfs.StatusOK || mismatch.Entries != 1 || mismatch.First != first || mismatch.Copy != 1 {
				t.Fatalf("mismatch %+v %v", mismatch, status)
			}
			if got := readAll(t, fs, mustOpen(t, fs, root, "file"), 5000, 5000); string(got) != string(pattern(5000, 1)) {
				t.Fatal("the second copy was read")
			}
			// Growing the file by clusters changes the FAT unit holding that entry.
			if _, status := fs.Write(mustOpen(t, fs, root, "file"), 5000, make([]byte, 8192)); status != ramfs.StatusOK {
				t.Fatal(status)
			}
			if mismatch, _ := fs.CompareFATs(); mismatch.Entries != 0 {
				t.Fatalf("still differs after a write: %+v", mismatch)
			}
			check(t, device)
		})
	}
}

func TestHealthReportsShortFATAndFewClusters(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 1})
	fs, _ := mounted(t, device)
	if health := fs.Health(); !health.FewClusters || health.ShortFAT || health.Degraded() {
		t.Fatalf("small volume: %+v", health)
	}
	put32(device.data[bpbFATSize32:], 8)
	fs, _ = mounted(t, device)
	if health := fs.Health(); !health.ShortFAT || fs.clusters != 8*512/4-2 {
		t.Fatalf("short FAT: %+v, %d clusters", health, fs.clusters)
	}
}

// TestBPBArithmeticCannotEscapeTheDevice drives parseBoot with fields at
// their extremes. Every combination must be refused or yield a geometry that
// lies inside the device.
func TestBPBArithmeticCannotEscapeTheDevice(t *testing.T) {
	capacity := uint64(64 << 20)
	extremes16 := []uint16{0, 1, 2, 31, 32, 0x7fff, 0xffff}
	extremes32 := []uint32{0, 1, 2, 0xffff, 0x0ffffff6, 0x0ffffff7, 0x10000000, 0xffffffff}
	var boot [unit]byte
	boot[510], boot[511], boot[bpbMedia] = 0x55, 0xaa, 0xf8
	for _, bytes := range []uint16{512, 4096} {
		for _, perCluster := range []byte{1, 128} {
			for _, reserved := range extremes16 {
				for _, fatSize := range extremes32 {
					for _, total := range extremes32 {
						for _, root := range extremes32 {
							put16(boot[bpbBytes:], bytes)
							boot[bpbPerCluster] = perCluster
							put16(boot[bpbReserved:], reserved)
							boot[bpbFATs] = 2
							put32(boot[bpbFATSize32:], fatSize)
							put32(boot[bpbTotal32:], total)
							put32(boot[bpbRootCluster:], root)
							geo, status := parseBoot(boot[:], capacity)
							if status != ramfs.StatusOK {
								continue
							}
							data := (geo.reserved + uint64(geo.copies)*geo.fatSectors) * geo.sectorSize
							end := data + geo.clusters*geo.perCluster*geo.sectorSize
							if end > capacity || geo.clusters*4+8 > geo.fatSectors*geo.sectorSize || uint64(geo.root-2) >= geo.clusters {
								t.Fatalf("accepted an escaping geometry: %+v", geo)
							}
						}
					}
				}
			}
		}
	}
}
