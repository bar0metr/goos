package partition

import (
	"fmt"
	"hash/crc32"
	"math/rand"
	"testing"
	"time"
)

type memoryDevice struct{ data []byte }

func (device *memoryDevice) ReadAt(offset uint64, target []byte) bool {
	return offset+uint64(len(target)) <= uint64(len(device.data)) && copy(target, device.data[offset:]) == len(target)
}
func (device *memoryDevice) WriteAt(offset uint64, data []byte) bool {
	return offset+uint64(len(data)) <= uint64(len(device.data)) && copy(device.data[offset:], data) == len(data)
}

func newDevice(size uint64) *memoryDevice { return &memoryDevice{data: make([]byte, size)} }

func scan(t *testing.T, device *memoryDevice, blockSize uint32) (*Table, Status) {
	t.Helper()
	var scanner Scanner
	table := &Table{}
	return table, scanner.Scan(device, uint64(len(device.data)), blockSize, table)
}

func mustScan(t *testing.T, device *memoryDevice, blockSize uint32) *Table {
	t.Helper()
	table, status := scan(t, device, blockSize)
	if status != StatusOK {
		t.Fatalf("scan: %v", status)
	}
	return table
}

var (
	diskGUID  = mustGUID("11111111-2222-3333-4444-555555555555")
	firstGUID = mustGUID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
	otherGUID = mustGUID("01234567-89AB-CDEF-0123-456789ABCDEF")
)

func mustGUID(text string) GUID {
	guid, ok := ParseGUID(text)
	if !ok {
		panic(text)
	}
	return guid
}

// gptDevice is a 64 MiB device with an EFI system partition, a basic-data
// partition and an unknown one.
func gptDevice(t *testing.T, blockSize uint32) *memoryDevice {
	t.Helper()
	device := newDevice(64 << 20)
	mib := uint64(1<<20) / uint64(blockSize)
	_, lastUsable := GPTUsable(uint64(len(device.data)), blockSize)
	if !WriteGPT(device, uint64(len(device.data)), blockSize, diskGUID, []GPTPartition{
		{Type: TypeEFISystem, Unique: otherGUID, First: mib, Last: 9*mib - 1, Name: "EFI System"},
		{Type: TypeBasicData, Unique: firstGUID, First: 9 * mib, Last: 40*mib - 1, Name: "Данные 😀"},
		{Type: mustGUID("0FC63DAF-8483-4772-8E79-3D69D8477DE4"), Unique: GUID{9}, First: 40 * mib, Last: lastUsable, Name: "linux"},
	}) {
		t.Fatal("write GPT")
	}
	return device
}

func TestEmptyAndRawDevices(t *testing.T) {
	device := newDevice(8 << 20)
	table := mustScan(t, device, 512)
	if table.Scheme != SchemeNone {
		t.Fatalf("scheme %v", table.Scheme)
	}
	if volume, status := Select(table, &Selector{}); status != StatusOK || volume.Start != 0 || volume.Length != 8<<20 {
		t.Fatalf("auto on a blank device: %+v %v", volume, status)
	}
	// A FAT boot sector at LBA 0 is a whole-device volume, even though its
	// boot code region has a signature and zeros where MBR entries would be.
	fatBootSector(device.data, 8<<20/512)
	if table = mustScan(t, device, 512); table.Scheme != SchemeRaw || table.Count != 0 {
		t.Fatalf("superfloppy: %v %d", table.Scheme, table.Count)
	}
	// exFAT and NTFS boot sectors are whole-device volumes too.
	for _, name := range []string{"EXFAT   ", "NTFS    "} {
		other := newDevice(8 << 20)
		copy(other.data[3:], name)
		other.data[510], other.data[511] = 0x55, 0xaa
		putMBREntry(other.data[mbrTable:], MBRPartition{Type: 0x07, Start: 2048, Count: 100})
		if table := mustScan(t, other, 512); table.Scheme != SchemeRaw {
			t.Fatalf("%q: %v", name, table.Scheme)
		}
	}
}

// fatBootSector writes a FAT32 boot sector as mkfs writes one.
func fatBootSector(data []byte, total uint32) {
	data[0], data[1], data[2] = 0xeb, 0x58, 0x90
	copy(data[3:11], "MSWIN4.1")
	put16(data[11:], 512)
	data[13] = 8
	put16(data[14:], 32)
	data[16] = 2
	data[21] = 0xf8
	put32(data[32:], total)
	put32(data[36:], 1000)
	put32(data[44:], 2)
	data[510], data[511] = 0x55, 0xaa
}

func TestMBR(t *testing.T) {
	device := newDevice(64 << 20)
	WriteMBR(device, 512, []MBRPartition{
		{Type: 0x83, Start: 2048, Count: 4096},
		{Type: 0x0c, Start: 8192, Count: 16384, Active: true},
		{Type: 0x0f, Start: 32768, Count: 1000},
		{Type: 0x0b, Start: 40000, Count: 1000},
	})
	ebr := device.data[32768*512 : 32769*512]
	putMBREntry(ebr[mbrTable:], MBRPartition{Type: 0x0c, Start: 1, Count: 400})
	putMBREntry(ebr[mbrTable+mbrEntrySize:], MBRPartition{Type: 0x0f, Start: 500, Count: 500})
	ebr[510], ebr[511] = 0x55, 0xaa
	ebr = device.data[33268*512 : 33269*512]
	putMBREntry(ebr[mbrTable:], MBRPartition{Type: 0x0b, Start: 1, Count: 400})
	ebr[510], ebr[511] = 0x55, 0xaa
	table := mustScan(t, device, 512)
	if table.Scheme != SchemeMBR || table.Count != 6 {
		t.Fatalf("scheme %v count %d", table.Scheme, table.Count)
	}
	want := []struct {
		index  uint32
		start  uint64
		length uint64
		kind   Kind
	}{{1, 2048 * 512, 4096 * 512, KindOther}, {2, 8192 * 512, 16384 * 512, KindData}, {3, 32768 * 512, 1000 * 512, KindExtended}, {4, 40000 * 512, 1000 * 512, KindData}, {5, 32769 * 512, 400 * 512, KindData}, {6, 33269 * 512, 400 * 512, KindData}}
	for i, want := range want {
		got := table.Partitions[i]
		if got.Index != want.index || got.Start != want.start || got.Length != want.length || got.Kind != want.kind {
			t.Fatalf("partition %d: %+v", i, got)
		}
	}
	// Auto takes the first FAT32 partition; number 1 is Linux, but a number
	// takes any type except the extended container; logical partitions use
	// Linux-compatible indices starting at five.
	for _, test := range []struct {
		selector Selector
		start    uint64
		status   Status
	}{
		{Selector{Kind: SelectAuto}, 8192 * 512, StatusOK},
		{Selector{Kind: SelectIndex, Index: 1}, 2048 * 512, StatusOK},
		{Selector{Kind: SelectIndex, Index: 4}, 40000 * 512, StatusOK},
		{Selector{Kind: SelectIndex, Index: 5}, 32769 * 512, StatusOK},
		{Selector{Kind: SelectIndex, Index: 6}, 33269 * 512, StatusOK},
		{Selector{Kind: SelectIndex, Index: 3}, 0, StatusUnsupported},
		{Selector{Kind: SelectIndex, Index: 7}, 0, StatusNotFound},
		{Selector{Kind: SelectGUID, GUID: firstGUID}, 0, StatusNotFound},
		{Selector{Kind: SelectWhole}, 0, StatusOK},
	} {
		volume, status := Select(table, &test.selector)
		if status != test.status || status == StatusOK && volume.Start != test.start {
			t.Errorf("%+v: %+v %v", test.selector.Kind, volume, status)
		}
	}
	// No FAT32 partition: auto finds nothing rather than guessing.
	WriteMBR(device, 512, []MBRPartition{{Type: 0x83, Start: 2048, Count: 4096}})
	if _, status := Select(mustScan(t, device, 512), &Selector{}); status != StatusNotFound {
		t.Fatalf("auto without a FAT partition: %v", status)
	}
}

func TestMBRCorruption(t *testing.T) {
	blocks := uint32(64 << 20 / 512)
	for name, test := range map[string]struct {
		partitions []MBRPartition
		scheme     Scheme
		status     Status
	}{
		"overlap":            {[]MBRPartition{{Type: 0x0c, Start: 2048, Count: 4096}, {Type: 0x0c, Start: 4000, Count: 10}}, SchemeNone, StatusCorrupt},
		"past the device":    {[]MBRPartition{{Type: 0x0c, Start: 2048, Count: blocks}}, SchemeNone, StatusCorrupt},
		"start past the end": {[]MBRPartition{{Type: 0x0c, Start: blocks + 1, Count: 1}}, SchemeNone, StatusCorrupt},
		"overflowing sum":    {[]MBRPartition{{Type: 0x0c, Start: 0xffffffff, Count: 0xffffffff}}, SchemeNone, StatusCorrupt},
		"starts at LBA 0":    {[]MBRPartition{{Type: 0x0c, Start: 0, Count: 100}}, SchemeNone, StatusCorrupt},
		"empty length":       {[]MBRPartition{{Type: 0x0c, Start: 100, Count: 0}}, SchemeNone, StatusCorrupt},
		"no used entry":      {[]MBRPartition{{Type: 0, Start: 100, Count: 10}}, SchemeNone, StatusOK},
	} {
		device := newDevice(64 << 20)
		WriteMBR(device, 512, test.partitions)
		table, status := scan(t, device, 512)
		if status != test.status || status == StatusOK && table.Scheme != test.scheme {
			t.Errorf("%s: %v %v", name, table.Scheme, status)
		}
	}
	// A boot indicator other than 0x00 or 0x80 means the sector is not a
	// partition table at all.
	device := newDevice(64 << 20)
	WriteMBR(device, 512, []MBRPartition{{Type: 0x0c, Start: 2048, Count: 4096}})
	device.data[mbrTable] = 0x12
	if table := mustScan(t, device, 512); table.Scheme != SchemeNone {
		t.Fatalf("bad boot indicator: %v", table.Scheme)
	}
}

func TestGPT(t *testing.T) {
	for _, blockSize := range []uint32{512, 4096} {
		t.Run(fmt.Sprint(blockSize), func(t *testing.T) {
			device := gptDevice(t, blockSize)
			table := mustScan(t, device, blockSize)
			if table.Scheme != SchemeGPT || table.Count != 3 || table.DiskGUID != diskGUID || table.GPT.Degraded() {
				t.Fatalf("scheme %v count %d disk %v health %+v", table.Scheme, table.Count, table.DiskGUID, table.GPT)
			}
			mib := uint64(1 << 20)
			data := table.Partitions[1]
			if data.Index != 2 || data.Start != 9*mib || data.Length != 31*mib || data.Kind != KindData || data.Unique != firstGUID || data.NameString() != "Данные 😀" {
				t.Fatalf("basic data: %+v %q", data, data.NameString())
			}
			if table.Partitions[0].Kind != KindEFI || table.Partitions[2].Kind != KindOther {
				t.Fatal("kinds")
			}
			name, _ := NameSelector("linux")
			for _, test := range []struct {
				selector Selector
				start    uint64
				status   Status
			}{
				// Auto skips the EFI system partition for basic data.
				{Selector{Kind: SelectAuto}, 9 * mib, StatusOK},
				{Selector{Kind: SelectIndex, Index: 1}, mib, StatusOK},
				{Selector{Kind: SelectGUID, GUID: firstGUID}, 9 * mib, StatusOK},
				{Selector{Kind: SelectGUID, GUID: diskGUID}, 0, StatusNotFound},
				{name, 40 * mib, StatusOK},
				{Selector{Kind: SelectName}, 0, StatusInvalidArgument},
				{Selector{Kind: 99}, 0, StatusInvalidArgument},
			} {
				volume, status := Select(table, &test.selector)
				if status != test.status || status == StatusOK && volume.Start != test.start {
					t.Errorf("%+v: %+v %v", test.selector.Kind, volume, status)
				}
			}
		})
	}
}

// reseal recomputes the CRCs of the GPT header at lba after a test has
// edited it or its entries, so that the damage under test is the only one.
func reseal(device *memoryDevice, blockSize uint32, lba uint64) {
	size := uint64(blockSize)
	header := device.data[lba*size : lba*size+size]
	entries := get64(header[72:]) * size
	length := uint64(get32(header[80:])) * uint64(get32(header[84:]))
	if entries/size != get64(header[72:]) || entries > uint64(len(device.data)) || length > uint64(len(device.data))-entries {
		return
	}
	put32(header[88:], crc32Update(0, device.data[entries:entries+length]))
	put32(header[16:], 0)
	put32(header[16:], crc32Update(0, header[:get32(header[12:])]))
}

// resealHeader recomputes only the header CRC, for a header whose entry
// array is not where it says.
func resealHeader(device *memoryDevice, blockSize uint32, lba uint64) {
	header := device.data[lba*uint64(blockSize):]
	put32(header[16:], 0)
	put32(header[16:], crc32Update(0, header[:get32(header[12:])]))
}

func TestGPTBackupAndCorruption(t *testing.T) {
	const blockSize = 512
	size := uint64(64 << 20)
	last := size/blockSize - 1
	entryOffset := func(copy uint64, index int) uint64 {
		if copy == 1 {
			return 2*blockSize + uint64(index)*128
		}
		return (last-32)*blockSize + uint64(index)*128
	}
	for name, test := range map[string]struct {
		damage func(device *memoryDevice)
		status Status
		health GPTHealth
	}{
		"primary header CRC": {func(device *memoryDevice) { device.data[blockSize+40]++ }, StatusOK, GPTHealth{UsedBackup: true}},
		"primary signature":  {func(device *memoryDevice) { device.data[blockSize] = 'X' }, StatusOK, GPTHealth{UsedBackup: true}},
		"primary entry CRC":  {func(device *memoryDevice) { device.data[entryOffset(1, 1)+60]++ }, StatusOK, GPTHealth{UsedBackup: true}},
		"backup header CRC":  {func(device *memoryDevice) { device.data[last*blockSize+40]++ }, StatusOK, GPTHealth{BackupInvalid: true}},
		"backup entry CRC":   {func(device *memoryDevice) { device.data[entryOffset(2, 0)+60]++ }, StatusOK, GPTHealth{BackupInvalid: true}},
		"both headers": {func(device *memoryDevice) {
			device.data[blockSize+40]++
			device.data[last*blockSize+40]++
		}, StatusCorrupt, GPTHealth{}},
		"both entry arrays": {func(device *memoryDevice) {
			device.data[entryOffset(1, 1)+60]++
			device.data[entryOffset(2, 1)+60]++
		}, StatusCorrupt, GPTHealth{}},
		"backup describes another disk": {func(device *memoryDevice) {
			device.data[last*blockSize+56]++
			reseal(device, blockSize, last)
		}, StatusOK, GPTHealth{BackupDiffers: true}},
		"partition outside the usable range": {func(device *memoryDevice) {
			for _, copy := range []uint64{1, 2} {
				put64(device.data[entryOffset(copy, 2)+40:], last)
			}
			reseal(device, blockSize, 1)
			reseal(device, blockSize, last)
		}, StatusCorrupt, GPTHealth{}},
		"overlapping partitions": {func(device *memoryDevice) {
			for _, copy := range []uint64{1, 2} {
				put64(device.data[entryOffset(copy, 1)+32:], 2048)
			}
			reseal(device, blockSize, 1)
			reseal(device, blockSize, last)
		}, StatusCorrupt, GPTHealth{}},
		"end before start": {func(device *memoryDevice) {
			for _, copy := range []uint64{1, 2} {
				put64(device.data[entryOffset(copy, 1)+40:], 100)
			}
			reseal(device, blockSize, 1)
			reseal(device, blockSize, last)
		}, StatusCorrupt, GPTHealth{}},
		"entry array past the device": {func(device *memoryDevice) {
			for _, lba := range []uint64{1, last} {
				put64(device.data[lba*blockSize+72:], last)
				resealHeader(device, blockSize, lba)
			}
		}, StatusCorrupt, GPTHealth{}},
		"huge entry count": {func(device *memoryDevice) {
			for _, lba := range []uint64{1, last} {
				put32(device.data[lba*blockSize+80:], 1<<30)
				put32(device.data[lba*blockSize+16:], 0)
				put32(device.data[lba*blockSize+16:], crc32Update(0, device.data[lba*blockSize:lba*blockSize+92]))
			}
		}, StatusCorrupt, GPTHealth{}},
		"entry array over the usable range": {func(device *memoryDevice) {
			put64(device.data[blockSize+72:], 2048)
			reseal(device, blockSize, 1)
		}, StatusOK, GPTHealth{UsedBackup: true}},
		"header naming another LBA": {func(device *memoryDevice) {
			put64(device.data[blockSize+24:], 2)
			reseal(device, blockSize, 1)
		}, StatusOK, GPTHealth{UsedBackup: true}},
		"entry size not a power of two": {func(device *memoryDevice) {
			put32(device.data[blockSize+84:], 130)
			reseal(device, blockSize, 1)
		}, StatusOK, GPTHealth{UsedBackup: true}},
		"nonzero reserved field": {func(device *memoryDevice) {
			device.data[blockSize+20] = 1
			reseal(device, blockSize, 1)
		}, StatusOK, GPTHealth{UsedBackup: true}},
		"revision 2": {func(device *memoryDevice) {
			device.data[blockSize+10] = 2
			reseal(device, blockSize, 1)
		}, StatusOK, GPTHealth{UsedBackup: true}},
	} {
		t.Run(name, func(t *testing.T) {
			device := gptDevice(t, blockSize)
			test.damage(device)
			before := string(device.data)
			table, status := scan(t, device, blockSize)
			if status != test.status {
				t.Fatalf("status %v, want %v", status, test.status)
			}
			if status == StatusOK && (table.GPT != test.health || table.Count != 3) {
				t.Fatalf("health %+v count %d, want %+v", table.GPT, table.Count, test.health)
			}
			if status != StatusOK && table.Count != 0 {
				t.Fatal("a refused table still lists partitions")
			}
			if string(device.data) != before {
				t.Fatal("scanning wrote to the device")
			}
		})
	}
}

// TestGrownImageKeepsItsTable: an image grown after partitioning has its
// backup header short of the last block; the table is still read, and the
// misplacement reported.
func TestGrownImageKeepsItsTable(t *testing.T) {
	device := gptDevice(t, 512)
	device.data = append(device.data, make([]byte, 8<<20)...)
	table := mustScan(t, device, 512)
	if table.Count != 3 || !table.GPT.BackupMisplaced || table.GPT.UsedBackup || table.GPT.BackupInvalid {
		t.Fatalf("grown image: %+v", table.GPT)
	}
	// With the primary gone, the backup is looked for at the last block,
	// where it is not: the table is corrupt.
	device.data[512] = 0
	if _, status := scan(t, device, 512); status != StatusCorrupt {
		t.Fatalf("grown image without its primary: %v", status)
	}
}

// TestProtectiveEntryInBootCode: a FAT boot sector whose boot code happens to
// hold a protective-MBR entry is still a whole-device volume.
func TestProtectiveEntryInBootCode(t *testing.T) {
	device := newDevice(8 << 20)
	fatBootSector(device.data, 8<<20/512)
	putMBREntry(device.data[mbrTable:], MBRPartition{Type: mbrTypeProtective, Start: 1, Count: 100})
	if table := mustScan(t, device, 512); table.Scheme != SchemeRaw {
		t.Fatalf("scheme %v", table.Scheme)
	}
	// Without the boot sector it is a GPT whose headers are missing.
	device = newDevice(8 << 20)
	WriteMBR(device, 512, []MBRPartition{{Type: mbrTypeProtective, Start: 1, Count: 100}})
	if _, status := scan(t, device, 512); status != StatusCorrupt {
		t.Fatalf("protective MBR without a GPT: %v", status)
	}
}

func TestScanRefusesBadArguments(t *testing.T) {
	var scanner Scanner
	var table Table
	for _, test := range []struct {
		capacity  uint64
		blockSize uint32
	}{{8 << 20, 256}, {8 << 20, 8192}, {8 << 20, 1000}, {8<<20 + 1, 512}, {100, 512}} {
		if status := scanner.Scan(newDevice(8<<20), test.capacity, test.blockSize, &table); status != StatusInvalidArgument {
			t.Errorf("%+v: %v", test, status)
		}
	}
	if status := scanner.Scan(nil, 8<<20, 512, &table); status != StatusInvalidArgument {
		t.Error("a nil reader was scanned")
	}
	failing := &failingReader{}
	if status := scanner.Scan(failing, 8<<20, 512, &table); status != StatusIOError {
		t.Errorf("unreadable device: %v", status)
	}
}

type failingReader struct{}

func (*failingReader) ReadAt(uint64, []byte) bool { return false }

func TestTooManyPartitionsIsUnsupported(t *testing.T) {
	device := newDevice(64 << 20)
	var partitions []GPTPartition
	for index := 0; index < MaxPartitions; index++ {
		partitions = append(partitions, GPTPartition{Type: TypeBasicData, Unique: GUID{byte(index)}, First: 100 + uint64(index)*10, Last: 109 + uint64(index)*10})
	}
	if !WriteGPT(device, 64<<20, 512, diskGUID, partitions) {
		t.Fatal("write")
	}
	if table := mustScan(t, device, 512); table.Count != MaxPartitions {
		t.Fatalf("%d partitions", table.Count)
	}
	// A 129th used entry, in an array of 256 slots.
	partitions = append(partitions, GPTPartition{Type: TypeBasicData, Unique: GUID{200}, First: 5000, Last: 5001})
	if !writeGPT(device, 64<<20, 512, diskGUID, partitions, 256) {
		t.Fatal("write")
	}
	if _, status := scan(t, device, 512); status != StatusUnsupported {
		t.Fatalf("129 partitions: %v", status)
	}
}

func TestCRCAndGUID(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for size := 0; size < 5000; size += 97 {
		data := make([]byte, size)
		random.Read(data)
		split := size / 3
		if got, want := crc32Update(crc32Update(0, data[:split]), data[split:]), crc32.ChecksumIEEE(data); got != want {
			t.Fatalf("crc of %d bytes: %08x, want %08x", size, got, want)
		}
	}
	if TypeBasicData.String() != "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7" || TypeEFISystem.String() != "C12A7328-F81F-11D2-BA4B-00A0C93EC93B" {
		t.Fatalf("%s %s", TypeBasicData, TypeEFISystem)
	}
	if guid, ok := ParseGUID("ebd0a0a2-b9e5-4433-87c0-68b6b72699c7"); !ok || guid != TypeBasicData {
		t.Fatal("lower-case parse")
	}
	for _, bad := range []string{"", "EBD0A0A2-B9E5-4433-87C0-68B6B72699C", "EBD0A0A2+B9E5-4433-87C0-68B6B72699C7", "GBD0A0A2-B9E5-4433-87C0-68B6B72699C7"} {
		if _, ok := ParseGUID(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestGPTNames(t *testing.T) {
	var entry [72]byte
	for _, name := range []string{"", "a", "Данные", "日本語", "😀😀", "exactly thirty-six characters long!!"} {
		entry = [72]byte{}
		if !putGPTName(entry[:], name) {
			t.Fatalf("%q refused", name)
		}
		var target [MaxName]byte
		if got := string(target[:gptName(entry[:], target[:])]); got != name {
			t.Fatalf("%q came back as %q", name, got)
		}
	}
	if putGPTName(entry[:], "thirty-seven characters is one too many") || putGPTName(entry[:], string([]byte{0xff})) {
		t.Fatal("an unrepresentable name was written")
	}
	// Unpaired surrogates and control characters leave the name empty.
	var target [MaxName]byte
	for _, units := range [][]uint16{{'a', 0xd800, 'b'}, {0xdc00}, {'a', 0x01}} {
		entry = [72]byte{}
		for i, unit := range units {
			put16(entry[2*i:], unit)
		}
		if size := gptName(entry[:], target[:]); size != 0 {
			t.Fatalf("%x decoded to %q", units, target[:size])
		}
	}
}

// TestRandomCorruptionNeverPanics damages the table areas of an MBR and a GPT
// device at random, many times, and scans each: every result must be a status,
// and an accepted table must lie inside the device.
func TestRandomCorruptionNeverPanics(t *testing.T) {
	const blockSize = 512
	gpt := newDevice(8 << 20)
	if !WriteGPT(gpt, 8<<20, blockSize, diskGUID, []GPTPartition{
		{Type: TypeEFISystem, Unique: otherGUID, First: 2048, Last: 4095, Name: "EFI"},
		{Type: TypeBasicData, Unique: firstGUID, First: 4096, Last: 12000, Name: "data"},
		{Type: TypeBasicData, Unique: GUID{7}, First: 12001, Last: 16000},
	}) {
		t.Fatal("write GPT")
	}
	pristineGPT := gpt.data
	pristineMBR := newDevice(8 << 20)
	WriteMBR(pristineMBR, blockSize, []MBRPartition{{Type: 0x0c, Start: 2048, Count: 10000}, {Type: 0x83, Start: 13000, Count: 1000}})
	last := uint64(len(pristineGPT))/blockSize - 1
	regions := [][2]uint64{{0, 34 * blockSize}, {(last - 32) * blockSize, (last + 1) * blockSize}}
	deadline := time.Now().Add(20 * time.Second)
	for seed := int64(0); seed < 2000 && time.Now().Before(deadline); seed++ {
		random := rand.New(rand.NewSource(seed))
		source := pristineGPT
		if seed%3 == 0 {
			source = pristineMBR.data
		}
		device := &memoryDevice{data: append([]byte(nil), source...)}
		for flips := 1 + random.Intn(16); flips > 0; flips-- {
			region := regions[random.Intn(len(regions))]
			if seed%5 == 0 {
				region = [2]uint64{0, 1024}
			}
			device.data[region[0]+uint64(random.Int63n(int64(region[1]-region[0])))] = byte(random.Intn(256))
		}
		// Some of the damage is resealed, so it passes the CRCs and has to
		// be caught by the structural checks.
		if seed%2 == 0 {
			reseal(device, blockSize, 1)
		}
		table, status := scan(t, device, blockSize)
		if status > StatusInvalidArgument {
			t.Fatalf("seed %d: status %d", seed, status)
		}
		for index := 0; status == StatusOK && index < table.Count; index++ {
			partition := table.Partitions[index]
			if partition.Length == 0 || partition.Start == 0 || partition.Start+partition.Length > uint64(len(device.data)) {
				t.Fatalf("seed %d: partition %+v escapes the device", seed, partition)
			}
		}
	}
}
