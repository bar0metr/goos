package fat32

import (
	"goos/kernel/ramfs"
	"testing"
	"time"
)

// fakeClock is a settable wall clock.
type fakeClock struct {
	now   time.Time
	known bool
}

func (clock *fakeClock) read() (int64, bool) { return clock.now.UnixNano(), clock.known }

func clocked(t *testing.T, device *memoryDevice, clock *fakeClock) (*FileSystem, ramfs.NodeID) {
	t.Helper()
	fs := &FileSystem{}
	fs.SetClock(clock.read)
	root, ok := fs.Mount(device)
	if !ok {
		t.Fatal("mount failed")
	}
	return fs, root
}

func metadata(t *testing.T, fs *FileSystem, id ramfs.NodeID) Metadata {
	t.Helper()
	value, status := fs.Metadata(id)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	return value
}

func at(value Time) time.Time { return time.Unix(0, value.UnixNanos).UTC() }

func TestTimeEncodingRoundTrip(t *testing.T) {
	for _, test := range []struct {
		in, modified, created time.Time
	}{
		{
			in:       time.Date(2026, 9, 26, 13, 45, 31, 123456789, time.UTC),
			modified: time.Date(2026, 9, 26, 13, 45, 30, 0, time.UTC),
			created:  time.Date(2026, 9, 26, 13, 45, 31, 120000000, time.UTC),
		},
		{
			in:       time.Date(2000, 2, 29, 23, 59, 59, 999999999, time.UTC),
			modified: time.Date(2000, 2, 29, 23, 59, 58, 0, time.UTC),
			created:  time.Date(2000, 2, 29, 23, 59, 59, 990000000, time.UTC),
		},
		{
			in:       time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
			created:  time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			in:       time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC),
			modified: time.Date(2107, 12, 31, 23, 59, 58, 0, time.UTC),
			created:  time.Date(2107, 12, 31, 23, 59, 59, 990000000, time.UTC),
		},
	} {
		stamp := encodeTime(test.in.UnixNano())
		if got := at(decodeTime(stamp.date, stamp.time, 0)); !got.Equal(test.modified) {
			t.Errorf("%v: modification %v, want %v", test.in, got, test.modified)
		}
		if got := at(decodeTime(stamp.date, stamp.time, stamp.tenths)); !got.Equal(test.created) {
			t.Errorf("%v: creation %v, want %v", test.in, got, test.created)
		}
	}
	// Every day FAT can hold survives a round trip through the calendar code.
	for day := time.Date(1980, 1, 1, 12, 0, 0, 0, time.UTC); day.Year() < 2108; day = day.AddDate(0, 0, 1) {
		stamp := encodeTime(day.UnixNano())
		if got := at(decodeTime(stamp.date, stamp.time, stamp.tenths)); !got.Equal(day) {
			t.Fatalf("%v came back as %v", day, got)
		}
	}
}

func TestStatTranslatesMetadataToNativeTypes(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 9, 29, 12, 34, 57, 120000000, time.UTC), known: true}
	fs, root := clocked(t, formattedDevice(), clock)
	name, _ := ramfs.MakeName([]byte("native.txt"))
	file, status := fs.CreateName(root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	stat, status := fs.Stat(file)
	if status != ramfs.StatusOK || !stat.Created.Available || !stat.Modified.Available || !stat.AttributesAvailable || stat.Attributes != ramfs.AttrArchive {
		t.Fatalf("native stat = %+v, %d", stat, status)
	}
	if stat.Created.Seconds != clock.now.Unix() || stat.Created.Nanoseconds != 120000000 || stat.Modified.Seconds != clock.now.Unix()-1 || stat.Modified.Nanoseconds != 0 {
		t.Fatalf("native timestamps = created %+v modified %+v", stat.Created, stat.Modified)
	}
}

func TestInvalidTimesAreUnknown(t *testing.T) {
	valid := encodeTime(time.Date(2020, 5, 17, 10, 20, 30, 0, time.UTC).UnixNano())
	for name, test := range map[string]struct {
		date, clock uint16
		tenths      uint8
	}{
		"zero date":     {0, valid.time, 0},
		"month 0":       {valid.date &^ (0x0f << 5), valid.time, 0},
		"month 13":      {valid.date&^(0x0f<<5) | 13<<5, valid.time, 0},
		"day 0":         {valid.date &^ 0x1f, valid.time, 0},
		"February 30":   {(2020-1980)<<9 | 2<<5 | 30, valid.time, 0},
		"February 29th": {(2021-1980)<<9 | 2<<5 | 29, valid.time, 0},
		"hour 24":       {valid.date, 24 << 11, 0},
		"minute 60":     {valid.date, 60 << 5, 0},
		"second 60":     {valid.date, 30, 0},
		"tenths 200":    {valid.date, valid.time, 200},
	} {
		if value := decodeTime(test.date, test.clock, test.tenths); value.Known {
			t.Errorf("%s decoded to %v", name, at(value))
		}
	}
}

func TestTimestampsFollowChanges(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	clock := &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), known: true}
	fs, root := clocked(t, device, clock)
	created := clock.now
	dir := mkdir(t, fs, root, "Каталог")
	file := writeFile(t, fs, dir, "notes.txt", "first")
	for name, id := range map[string]ramfs.NodeID{"directory": dir, "file": file} {
		value := metadata(t, fs, id)
		if !value.Created.Known || !at(value.Created).Equal(created) || !at(value.Modified).Equal(created) ||
			!at(value.Accessed).Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("%s: %+v", name, value)
		}
	}
	// "." and ".." carry the new directory's times.
	key := entryClusterOf(t, fs, dir)
	sub, _ := fs.openDirectory(key)
	dot, _ := fs.readEntry(&sub, 0)
	if value := metadataOf(dot); !at(value.Created).Equal(created) {
		t.Fatalf("'.' created %v", at(value.Created))
	}

	// A write changes the modification time and access date, not creation.
	clock.now = time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC)
	if _, status := fs.Write(file, 5, []byte(" and second")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	value := metadata(t, fs, file)
	if !at(value.Created).Equal(created) || !at(value.Modified).Equal(clock.now) ||
		!at(value.Accessed).Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("after a write: %+v", value)
	}
	// So does an overwrite that does not grow the file, and truncation.
	clock.now = clock.now.Add(time.Hour)
	fs.Write(file, 0, []byte("F"))
	if value := metadata(t, fs, file); !at(value.Modified).Equal(clock.now) {
		t.Fatalf("overwrite left %v", at(value.Modified))
	}
	clock.now = clock.now.Add(time.Hour)
	fs.Truncate(file, 2)
	if value := metadata(t, fs, file); !at(value.Modified).Equal(clock.now) {
		t.Fatalf("truncate left %v", at(value.Modified))
	}
	// A read records nothing.
	before := string(device.data)
	clock.now = clock.now.Add(24 * time.Hour)
	readFile(t, fs, file)
	fs.Flush()
	if string(device.data) != before {
		t.Fatal("a read wrote metadata")
	}

	// Creating, renaming and removing an entry changes its directory's
	// modification time.
	for _, change := range []func(){
		func() { writeFile(t, fs, dir, "second", "") },
		func() { fs.RenameName(mustOpen(t, fs, dir, "second"), dir, mustName(t, "renamed")) },
		func() { fs.UnlinkName(dir, mustName(t, "renamed")) },
	} {
		clock.now = clock.now.Add(time.Minute)
		change()
		if value := metadata(t, fs, mustOpen(t, fs, root, "каталог")); !at(value.Modified).Equal(clock.now) {
			t.Fatalf("directory modified %v, want %v", at(value.Modified), clock.now)
		}
	}
	// A move changes both directories.
	other := mkdir(t, fs, root, "other")
	clock.now = clock.now.Add(time.Minute)
	if status := fs.RenameName(mustOpen(t, fs, dir, "notes.txt"), other, mustName(t, "moved.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	for _, name := range []string{"каталог", "other"} {
		if value := metadata(t, fs, mustOpen(t, fs, root, name)); !at(value.Modified).Equal(clock.now) {
			t.Fatalf("%s modified %v", name, at(value.Modified))
		}
	}
	// The moved file keeps its own times.
	if value := metadata(t, fs, mustOpen(t, fs, other, "moved.txt")); !at(value.Created).Equal(created) {
		t.Fatalf("moved file created %v", at(value.Created))
	}
	if value := metadata(t, fs, root); value.Created.Known || value.Attributes != 0 {
		t.Fatalf("root metadata %+v", value)
	}
	check(t, device)
}

// TestNoClockRecordsNothing covers GoOS today: there is no wall clock, so a
// new entry's times are zero — unknown — and a change leaves a foreign
// volume's times as they were.
func TestNoClockRecordsNothing(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	stamped := &fakeClock{now: time.Date(2019, 7, 1, 8, 0, 0, 0, time.UTC), known: true}
	fs, root := clocked(t, device, stamped)
	id := writeFile(t, fs, root, "foreign", "x")
	for _, clock := range []*fakeClock{{known: false}, nil} {
		var fs FileSystem
		if clock != nil {
			fs.SetClock(clock.read)
		}
		root, _ := fs.Mount(device)
		id = mustOpen(t, &fs, root, "foreign")
		fs.Write(id, 1, []byte("more"))
		fs.Truncate(id, 3)
		value := metadata(t, &fs, id)
		if !at(value.Created).Equal(stamped.now) || !at(value.Modified).Equal(stamped.now) {
			t.Fatalf("a change without a clock rewrote the times: %+v", value)
		}
		fresh := writeFile(t, &fs, root, "fresh", "")
		if value := metadata(t, &fs, fresh); value.Created.Known || value.Modified.Known || value.Accessed.Known {
			t.Fatalf("a new entry got times without a clock: %+v", value)
		}
		fs.UnlinkName(root, mustName(t, "fresh"))
	}
	check(t, device)
}

// setAttributes writes raw attribute bits on the short entry of a node.
func setAttributes(t *testing.T, fs *FileSystem, id ramfs.NodeID, bits byte) {
	t.Helper()
	dir, index, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	entry[11] = bits
	if fs.writeEntry(&dir, index, entry) != ramfs.StatusOK {
		t.Fatal("write failed")
	}
}

func rawAttributes(t *testing.T, fs *FileSystem, id ramfs.NodeID) byte {
	t.Helper()
	_, _, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	return entry[11]
}

func TestAttributesArePreservedAndHonoured(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	fs, root := mounted(t, device)
	id := writeFile(t, fs, root, "Locked File.txt", "keep")
	if bits := rawAttributes(t, fs, id); bits != attrArchive {
		t.Fatalf("new file attributes %#x", bits)
	}
	// Read-only, hidden, system, and the two bits FAT does not define.
	setAttributes(t, fs, id, attrReadOnly|attrHidden|attrSystem|0x40|0x80)
	if value := metadata(t, fs, id); value.Attributes != AttrReadOnly|AttrHidden|AttrSystem {
		t.Fatalf("native attributes %#x", value.Attributes)
	}
	if _, status := fs.Write(id, 0, []byte("x")); status != ramfs.StatusPermissionDenied {
		t.Fatalf("write to a read-only file: %v", status)
	}
	if status := fs.Truncate(id, 0); status != ramfs.StatusPermissionDenied {
		t.Fatalf("truncate of a read-only file: %v", status)
	}
	if got := readFile(t, fs, id); got != "keep" {
		t.Fatalf("read-only file reads %q", got)
	}
	// Rename keeps every bit, across directories too.
	dir := mkdir(t, fs, root, "Dir")
	if status := fs.RenameName(id, dir, mustName(t, "Still Locked.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	id = mustOpen(t, fs, dir, "still locked.txt")
	if bits := rawAttributes(t, fs, id); bits != attrReadOnly|attrHidden|attrSystem|0x40|0x80 {
		t.Fatalf("rename changed attributes to %#x", bits)
	}
	// Clearing read-only makes it writable; a write sets ARCHIVE and keeps
	// the rest.
	setAttributes(t, fs, id, attrHidden|attrSystem|0x40)
	if _, status := fs.Write(id, 0, []byte("K")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if bits := rawAttributes(t, fs, id); bits != attrHidden|attrSystem|0x40|attrArchive {
		t.Fatalf("write left attributes %#x", bits)
	}
	// Read-only files can still be removed, as in Linux.
	setAttributes(t, fs, id, attrReadOnly)
	if status := fs.UnlinkName(dir, mustName(t, "Still Locked.txt")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	// Read-only means nothing on a directory: it still takes entries.
	setAttributes(t, fs, dir, attrDirectory|attrReadOnly)
	writeFile(t, fs, dir, "inside", "x")
	if value := metadata(t, fs, dir); value.Attributes&AttrReadOnly != 0 {
		t.Fatal("a directory reported read-only")
	}
	check(t, device)
}

func TestVolumeLabelIsNotAFile(t *testing.T) {
	fs, root := mounted(t, formattedDevice())
	dir, _ := fs.openDirectory(0)
	var label [32]byte
	copy(label[:11], "MY VOLUME  ")
	label[11] = attrVolume | attrArchive
	fs.writeEntry(&dir, 0, label)
	writeFile(t, fs, root, "file", "x")
	if names := listNames(t, fs, root); len(names) != 1 || names[0] != "file" {
		t.Fatalf("listed %q", names)
	}
	if _, status := fs.OpenName(root, mustName(t, "MY VOLUME")); status != ramfs.StatusNotFound {
		t.Fatalf("label opened: %v", status)
	}
	// A create that needs the label's slot does not take it.
	if label2 := rootEntry(t, fs, 0); label2[11]&attrVolume == 0 {
		t.Fatal("the label was overwritten")
	}
	if _, status := fs.Stat(dir.node(0)); status != ramfs.StatusNotFound {
		t.Fatalf("label stat: %v", status)
	}
}

// TestDirectoryTimeIsBestEffort: a directory whose entry cannot be found — its
// ".." is damaged — still takes a create; only its time is not recorded.
func TestDirectoryTimeIsBestEffort(t *testing.T) {
	device := formatted(t, 8<<20, Layout{SectorsPerCluster: 1, FATs: 2})
	clock := &fakeClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), known: true}
	fs, root := clocked(t, device, clock)
	dir := mkdir(t, fs, root, "dir")
	offset := fs.clusterOffset(entryClusterOf(t, fs, dir)) + entrySize
	put16(device.data[offset+26:], 777)
	fs, root = clocked(t, device, clock)
	if _, status := fs.CreateName(mustOpen(t, fs, root, "dir"), mustName(t, "inside")); status != ramfs.StatusOK {
		t.Fatalf("create under a damaged '..': %v", status)
	}
}

func TestFilesystemInfoUses64BitCapacityAndLiveFreeCount(t *testing.T) {
	fs := &FileSystem{mounted: true, freeKnown: true, clusters: 2_000_000, free: 1_500_000, clusterSize: 4096, readOnly: true}
	info, status := fs.FilesystemInfo()
	if status != ramfs.StatusOK || info.Type != ramfs.FilesystemFAT32 || info.TotalBytes != 8_192_000_000 || info.FreeBytes != 6_144_000_000 || info.AvailableBytes != info.FreeBytes || !info.ReadOnly {
		t.Fatalf("info=%+v status=%v", info, status)
	}
	if _, ok := checkedCapacity(^uint64(0), 2); ok {
		t.Fatal("overflowing capacity accepted")
	}
}

func TestVolumeExposesFilesystemInfo(t *testing.T) {
	volume := &Volume{}
	if info, status := volume.FilesystemInfo(); status != ramfs.StatusIOError || info.Type != 0 {
		t.Fatalf("unmounted info=%+v status=%v", info, status)
	}
	if !volume.Mount(formattedDevice()) {
		t.Fatal("mount")
	}
	info, status := volume.FilesystemInfo()
	if status != ramfs.StatusOK || info.Type != ramfs.FilesystemFAT32 || info.TotalBytes == 0 || !info.CapacityAvailable {
		t.Fatalf("mounted info=%+v status=%v", info, status)
	}
}
