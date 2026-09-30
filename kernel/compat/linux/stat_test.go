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
			FileStatus{Device: 5, Inode: 9, Mode: 0o100644, Size: 12, Seconds: 50, Nanoseconds: 6}, 0},
		{"directory", vfs.Reply{Kind: ramfs.KindDirectory, Node: 2, MountID: 5},
			FileStatus{Device: 5, Inode: 2, Mode: 0o040755}, 0},
		{"read-only file", vfs.Reply{Kind: ramfs.KindFile, Node: 9, Attributes: ramfs.AttrReadOnly, AttributesAvailable: true},
			FileStatus{Inode: 9, Mode: 0o100444}, 0},
		{"read-only directory", vfs.Reply{Kind: ramfs.KindDirectory, Node: 9, Attributes: ramfs.AttrReadOnly | ramfs.AttrDirectory, AttributesAvailable: true},
			FileStatus{Inode: 9, Mode: 0o040555}, 0},
		{"hidden is not read-only", vfs.Reply{Kind: ramfs.KindFile, Node: 9, Attributes: ramfs.AttrHidden, AttributesAvailable: true},
			FileStatus{Inode: 9, Mode: 0o100644}, 0},
		{"creation time only", vfs.Reply{Kind: ramfs.KindFile, Node: 9, Created: created},
			FileStatus{Inode: 9, Mode: 0o100644, Seconds: 40, Nanoseconds: 3}, 0},
		{"zero identity", vfs.Reply{Kind: ramfs.KindFile}, FileStatus{Inode: 1, Mode: 0o100644}, 0},
		{"unknown kind", vfs.Reply{Node: 9}, FileStatus{}, abi.EIO},
	}
	for _, test := range cases {
		got, errno := StatusOfReply(&test.reply)
		if got != test.want || errno != test.errno {
			t.Errorf("%s: %+v errno=%d, want %+v errno=%d", test.label, got, errno, test.want, test.errno)
		}
	}
	first, second := StreamStatus(0), StreamStatus(3)
	if first.Mode != 0o010600 || first.Inode == 0 || first.Inode == second.Inode || first.Device != StreamDevice || first.Size != 0 {
		t.Fatalf("stream status %+v %+v", first, second)
	}
}

func TestStatFlags(t *testing.T) {
	for flags, want := range map[uint64]struct {
		empty bool
		errno abi.Errno
	}{
		0:       {false, 0},
		0x100:   {false, 0},
		0x800:   {false, 0},
		0x1000:  {true, 0},
		0x1900:  {true, 0},
		0x200:   {false, abi.EINVAL}, // AT_REMOVEDIR
		0x400:   {false, abi.EINVAL}, // AT_SYMLINK_FOLLOW
		1 << 40: {false, abi.EINVAL},
	} {
		if empty, errno := StatFlags(flags); empty != want.empty || errno != want.errno {
			t.Errorf("flags %#x: empty=%v errno=%d", flags, empty, errno)
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
