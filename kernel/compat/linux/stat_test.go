package linux

import (
	"encoding/binary"
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// statField reads one field of an encoded struct stat by the offset and width
// the Linux x86_64 UAPI header gives it. The numbers are repeated here on
// purpose: the test must not share the encoder's constants.
func statField(encoded *[StatSize]byte, offset int, width int) uint64 {
	if width == 4 {
		return uint64(binary.LittleEndian.Uint32(encoded[offset:]))
	}
	return binary.LittleEndian.Uint64(encoded[offset:])
}

func TestEncodeStatMatchesTheLinuxLayout(t *testing.T) {
	var encoded [StatSize]byte
	for index := range encoded {
		encoded[index] = 0xff
	}
	EncodeStat(&encoded, FileStatus{Device: 0x100000002, Inode: 77, Mode: abi.ModeRegular | 0o644, Size: 1025, Seconds: 1790000000, Nanoseconds: 123456789})
	fields := []struct {
		name          string
		offset, width int
		want          uint64
	}{
		{"st_dev", 0, 8, 0x100000002},
		{"st_ino", 8, 8, 77},
		{"st_nlink", 16, 8, 1},
		{"st_mode", 24, 4, 0o100644},
		{"st_uid", 28, 4, 1000},
		{"st_gid", 32, 4, 1000},
		{"__pad0", 36, 4, 0},
		{"st_rdev", 40, 8, 0},
		{"st_size", 48, 8, 1025},
		{"st_blksize", 56, 8, 4096},
		{"st_blocks", 64, 8, 3},
		{"st_atime", 72, 8, 1790000000},
		{"st_atime_nsec", 80, 8, 123456789},
		{"st_mtime", 88, 8, 1790000000},
		{"st_mtime_nsec", 96, 8, 123456789},
		{"st_ctime", 104, 8, 1790000000},
		{"st_ctime_nsec", 112, 8, 123456789},
		{"__unused[0]", 120, 8, 0},
		{"__unused[1]", 128, 8, 0},
		{"__unused[2]", 136, 8, 0},
	}
	covered := 0
	for _, field := range fields {
		if got := statField(&encoded, field.offset, field.width); got != field.want {
			t.Errorf("%s at %d = %d, want %d", field.name, field.offset, got, field.want)
		}
		if field.offset != covered {
			t.Fatalf("%s starts at %d, previous field ended at %d", field.name, field.offset, covered)
		}
		covered += field.width
	}
	if covered != StatSize || StatSize != 144 {
		t.Fatalf("fields cover %d bytes of %d", covered, StatSize)
	}
}

func TestStatBlocksRoundUp(t *testing.T) {
	var encoded [StatSize]byte
	for size, want := range map[uint64]uint64{0: 0, 1: 1, 511: 1, 512: 1, 513: 2, 4096: 8, ^uint64(0): 1 << 55} {
		EncodeStat(&encoded, FileStatus{Size: size})
		if got := statField(&encoded, 64, 8); got != want {
			t.Errorf("size %d: %d blocks, want %d", size, got, want)
		}
	}
}

func TestStatusOfReply(t *testing.T) {
	modified := ramfs.Timestamp{Seconds: 50, Nanoseconds: 6, Available: true}
	created := ramfs.Timestamp{Seconds: 40, Nanoseconds: 3, Available: true}
	cases := []struct {
		label string
		reply vfs.Reply
		want  FileStatus
		errno abi.Errno
	}{
		{"writable file", vfs.Reply{Kind: ramfs.KindFile, Node: 9, MountID: 5, Size: 12, Modified: modified, Created: created},
			FileStatus{Device: 5, Inode: 9, Mode: 0o100644, Size: 12, Seconds: 50, Nanoseconds: 6, Born: true, BirthSeconds: 40, BirthNanosecond: 3}, 0},
		{"directory", vfs.Reply{Kind: ramfs.KindDirectory, Node: 2, MountID: 5},
			FileStatus{Device: 5, Inode: 2, Mode: 0o040755}, 0},
		{"read-only file", vfs.Reply{Kind: ramfs.KindFile, Node: 9, Attributes: ramfs.AttrReadOnly, AttributesAvailable: true},
			FileStatus{Inode: 9, Mode: 0o100444}, 0},
		{"read-only directory", vfs.Reply{Kind: ramfs.KindDirectory, Node: 9, Attributes: ramfs.AttrReadOnly | ramfs.AttrDirectory, AttributesAvailable: true},
			FileStatus{Inode: 9, Mode: 0o040555}, 0},
		{"hidden is not read-only", vfs.Reply{Kind: ramfs.KindFile, Node: 9, Attributes: ramfs.AttrHidden, AttributesAvailable: true},
			FileStatus{Inode: 9, Mode: 0o100644}, 0},
		{"creation time only", vfs.Reply{Kind: ramfs.KindFile, Node: 9, Created: created},
			FileStatus{Inode: 9, Mode: 0o100644, Seconds: 40, Nanoseconds: 3, Born: true, BirthSeconds: 40, BirthNanosecond: 3}, 0},
		{"zero identity", vfs.Reply{Kind: ramfs.KindFile}, FileStatus{Inode: 1, Mode: 0o100644}, 0},
		{"unknown kind", vfs.Reply{Node: 9}, FileStatus{}, abi.EIO},
		{"symbolic link", vfs.Reply{Kind: ramfs.KindSymlink, Node: 9, Size: 8}, FileStatus{Inode: 9, Mode: 0o120777, Size: 8}, 0},
		{"special without metadata", vfs.Reply{Kind: ramfs.KindSpecial, Node: 9}, FileStatus{}, abi.EIO},
		{"ext4 device", vfs.Reply{Kind: ramfs.KindSpecial, Node: 9, Attributes: ramfs.AttrReadOnly, AttributesAvailable: true,
			Modified: ramfs.Timestamp{Seconds: 50, Available: true},
			Unix: ramfs.Unix{Mode: 0o20640, UID: 4, GID: 5, Links: 2, Device: 0x103, Available: true,
				Accessed: ramfs.Timestamp{Seconds: 60, Nanoseconds: 1, Available: true}, Changed: ramfs.Timestamp{Seconds: 70, Nanoseconds: 2, Available: true}}},
			FileStatus{Inode: 9, Mode: 0o20640, RawDevice: 0x103, Seconds: 50, Stored: true, Links: 2, UID: 4, GID: 5,
				AccessSeconds: 60, AccessNanoseconds: 1, ChangeSeconds: 70, ChangeNanoseconds: 2}, 0},
	}
	for _, test := range cases {
		got, errno := StatusOfReply(&test.reply)
		if got != test.want || errno != test.errno {
			t.Errorf("%s: %+v errno=%d, want %+v errno=%d", test.label, got, errno, test.want, test.errno)
		}
	}
	first, second := StreamStatus(1), StreamStatus(4)
	if first.Mode != 0o010600 || first.Inode == 0 || first.Inode == second.Inode || first.Device != StreamDevice || first.Size != 0 {
		t.Fatalf("stream status %+v %+v", first, second)
	}
}

func TestStatFlags(t *testing.T) {
	for flags, want := range map[uint64]struct {
		empty    bool
		noFollow bool
		errno    abi.Errno
	}{
		0:       {false, false, 0},
		0x100:   {false, true, 0},
		0x800:   {false, false, 0},
		0x1000:  {true, false, 0},
		0x1900:  {true, true, 0},
		0x200:   {false, false, abi.EINVAL}, // AT_REMOVEDIR
		0x400:   {false, false, abi.EINVAL}, // AT_SYMLINK_FOLLOW
		1 << 40: {false, false, abi.EINVAL},
	} {
		if empty, noFollow, errno := StatFlags(flags); empty != want.empty || noFollow != want.noFollow || errno != want.errno {
			t.Errorf("flags %#x: empty=%v nofollow=%v errno=%d", flags, empty, noFollow, errno)
		}
	}
}

// FuzzEncodeStat checks that every input lands in its own field, the constant
// fields hold, and nothing else in the structure is set.
func FuzzEncodeStat(fuzz *testing.F) {
	fuzz.Add(uint64(1), uint64(2), uint32(0o100644), uint64(3), int64(4), uint32(5))
	fuzz.Add(^uint64(0), ^uint64(0), ^uint32(0), ^uint64(0), int64(-1), ^uint32(0))
	fuzz.Fuzz(func(t *testing.T, device, inode uint64, mode uint32, size uint64, seconds int64, nanoseconds uint32) {
		var encoded [StatSize]byte
		for index := range encoded {
			encoded[index] = 0xa5
		}
		EncodeStat(&encoded, FileStatus{Device: device, Inode: inode, Mode: mode, Size: size, Seconds: seconds, Nanoseconds: nanoseconds})
		blocks := size / 512
		if size%512 != 0 {
			blocks++
		}
		want := [...]uint64{device, inode, 1, uint64(mode) | 1000<<32, 1000, 0, size, 4096, blocks,
			uint64(seconds), uint64(nanoseconds), uint64(seconds), uint64(nanoseconds), uint64(seconds), uint64(nanoseconds), 0, 0, 0}
		for index, value := range want {
			if got := binary.LittleEndian.Uint64(encoded[index*8:]); got != value {
				t.Fatalf("word %d = %#x, want %#x", index, got, value)
			}
		}
	})
}

func TestAccessErrno(t *testing.T) {
	cases := []struct {
		requested uint64
		mode      uint32
		want      abi.Errno
	}{
		{0, 0o100644, 0},
		{4, 0o100644, 0},
		{6, 0o100644, 0},
		{1, 0o100644, abi.EACCES}, // a file has no execute bit
		{7, 0o040755, 0},
		{2, 0o100444, abi.EACCES}, // read-only
		{2, 0o040555, abi.EACCES},
		{5, 0o040555, 0},
		{8, 0o100644, abi.EINVAL},
	}
	for _, test := range cases {
		if got := AccessErrno(test.requested, test.mode); got != test.want {
			t.Errorf("access %d on %#o: errno %d, want %d", test.requested, test.mode, got, test.want)
		}
	}
}

func TestEncodeStatxMatchesTheLinuxLayout(t *testing.T) {
	var wire [StatxSize]byte
	for index := range wire {
		wire[index] = 0xee
	}
	status := FileStatus{
		Device: 0x0000_0123_0004_5678, Inode: 0x1122334455667788, Mode: abi.ModeRegular | 0o644, Size: 1025,
		Seconds: 1_700_000_000, Nanoseconds: 123456789, Born: true, BirthSeconds: 1_600_000_000, BirthNanosecond: 42,
	}
	EncodeStatx(&wire, status)
	u16, u32, u64 := binary.LittleEndian.Uint16, binary.LittleEndian.Uint32, binary.LittleEndian.Uint64
	major, minor := DeviceParts(status.Device)
	for name, got := range map[string][2]uint64{
		"stx_mask":      {uint64(u32(wire[0:])), 0xfff},
		"stx_blksize":   {uint64(u32(wire[4:])), StatBlockSize},
		"stx_attrs":     {u64(wire[8:]), 0},
		"stx_nlink":     {uint64(u32(wire[16:])), 1},
		"stx_uid":       {uint64(u32(wire[20:])), uint64(StatUID)},
		"stx_gid":       {uint64(u32(wire[24:])), uint64(StatGID)},
		"stx_mode":      {uint64(u16(wire[28:])), uint64(abi.ModeRegular | 0o644)},
		"padding":       {uint64(u16(wire[30:])), 0},
		"stx_ino":       {u64(wire[32:]), status.Inode},
		"stx_size":      {u64(wire[40:]), 1025},
		"stx_blocks":    {u64(wire[48:]), 3},
		"stx_atime":     {u64(wire[64:]), 1_700_000_000},
		"stx_atime_ns":  {uint64(u32(wire[72:])), 123456789},
		"stx_btime":     {u64(wire[80:]), 1_600_000_000},
		"stx_btime_ns":  {uint64(u32(wire[88:])), 42},
		"stx_ctime":     {u64(wire[96:]), 1_700_000_000},
		"stx_mtime":     {u64(wire[112:]), 1_700_000_000},
		"stx_mtime_ns":  {uint64(u32(wire[120:])), 123456789},
		"stx_rdev":      {u64(wire[128:]), 0},
		"stx_dev_major": {uint64(u32(wire[136:])), uint64(major)},
		"stx_dev_minor": {uint64(u32(wire[140:])), uint64(minor)},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %#x, want %#x", name, got[0], got[1])
		}
	}
	for index := 144; index < StatxSize; index++ {
		if wire[index] != 0 {
			t.Fatalf("byte %d of the unused tail is %#x", index, wire[index])
		}
	}
	// Without a creation time the mask leaves STATX_BTIME out and the field
	// stays zero.
	status.Born = false
	EncodeStatx(&wire, status)
	if u32(wire[0:]) != 0x7ff || u64(wire[80:]) != 0 {
		t.Fatalf("mask=%#x btime=%d without a creation time", u32(wire[0:]), u64(wire[80:]))
	}
	// A character device reports its number in the two rdev halves.
	EncodeStatx(&wire, DeviceStatus(DeviceNull))
	if u32(wire[128:]) != 1 || u32(wire[132:]) != 3 || u16(wire[28:]) != uint16(abi.ModeCharacter|0o666) {
		t.Fatalf("/dev/null: rdev %d:%d mode %#o", u32(wire[128:]), u32(wire[132:]), u16(wire[28:]))
	}
}

func TestDevicePartsInvertMakedev(t *testing.T) {
	makedev := func(major, minor uint32) uint64 {
		return uint64(major&0xfffff000)<<32 | uint64(major&0xfff)<<8 | uint64(minor&0xffffff00)<<12 | uint64(minor&0xff)
	}
	for _, device := range []uint64{0, 1, 0x103, 0x500, 0xfd00, 0x0000_0001_0000_0002, 0x1234_5678_9abc_def0, ^uint64(0)} {
		major, minor := DeviceParts(device)
		if makedev(major, minor) != device {
			t.Errorf("device %#x splits to %#x:%#x, which makes %#x", device, major, minor, makedev(major, minor))
		}
	}
	if major, minor := DeviceParts(0x103); major != 1 || minor != 3 {
		t.Fatalf("0x103 = %d:%d", major, minor)
	}
}

func TestStatxFlags(t *testing.T) {
	for _, test := range []struct {
		flags, mask uint64
		empty       bool
		errno       abi.Errno
	}{
		{0, 0x7ff, false, 0},
		{abi.AtEmptyPath, 0xfff, true, 0},
		{abi.AtSymlinkNoFollow | abi.AtNoAutomount, 0, false, 0},
		{0x2000, 0x7ff, false, 0},                  // AT_STATX_FORCE_SYNC
		{0x4000 | abi.AtEmptyPath, 0x7ff, true, 0}, // AT_STATX_DONT_SYNC
		{0x6000, 0x7ff, false, abi.EINVAL},
		{0, 0x80000000, false, abi.EINVAL},
		{0x10000000, 0x7ff, false, abi.EINVAL},
		{0, 0x7fffffff, false, 0},
	} {
		empty, noFollow, errno := StatxFlags(test.flags, test.mask)
		if empty != test.empty || errno != test.errno || noFollow != (errno == 0 && test.flags&abi.AtSymlinkNoFollow != 0) {
			t.Errorf("flags %#x mask %#x: empty=%v errno=%d", test.flags, test.mask, empty, errno)
		}
	}
}

func TestStoredInodeAndBlocks(t *testing.T) {
	reply := vfs.Reply{Kind: ramfs.KindFile, Node: 0x6f6f7367<<32 | 64, Size: 1048577, MountID: 3,
		Unix: ramfs.Unix{Mode: 0o100644, Links: 1, Inode: 64, Blocks: 8, Available: true}}
	status, errno := StatusOfReply(&reply)
	if errno != 0 || status.Inode != 64 || status.Device != 3 {
		t.Fatalf("status %+v", status)
	}
	var encoded [StatSize]byte
	EncodeStat(&encoded, status)
	if binary.LittleEndian.Uint64(encoded[statBlocks:]) != 8 || binary.LittleEndian.Uint64(encoded[statInode:]) != 64 {
		t.Fatal("st_blocks or st_ino")
	}
	var extended [StatxSize]byte
	EncodeStatx(&extended, status)
	if binary.LittleEndian.Uint64(extended[statxBlocks:]) != 8 || binary.LittleEndian.Uint64(extended[statxInode:]) != 64 {
		t.Fatal("stx_blocks or stx_ino")
	}
	for status, want := range map[vfs.Status]abi.Errno{vfs.StatusOK: 0, vfs.StatusUnsupported: abi.EINVAL, vfs.StatusBackendError: abi.EIO, vfs.StatusPeerDied: abi.EIO} {
		if got := SyncErrno(status); got != want {
			t.Fatalf("sync status %v: errno %d", status, got)
		}
	}
	if VFSErrno(vfs.StatusCrossDevice, VFSPermissionAccess, abi.ENOMEM) != abi.EXDEV || MutationErrno(vfs.StatusCrossDevice) != abi.EXDEV {
		t.Fatal("EXDEV")
	}
}
