package linux

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"goos/internal/limits"
	"goos/internal/timekeeping"
	"goos/kernel/compat/linux/abi"
	"goos/kernel/ramfs"
)

func TestEncodeDirentMatchesTheLinuxLayout(t *testing.T) {
	var buffer [64]byte
	for index := range buffer {
		buffer[index] = 0xff
	}
	size, ok := EncodeDirent(buffer[:], 77, 5, ramfs.KindDirectory, []byte("abc"))
	// 19 header bytes, 3 of name, 1 terminator: 23, padded to 24.
	if !ok || size != 24 {
		t.Fatalf("size=%d ok=%v", size, ok)
	}
	if binary.LittleEndian.Uint64(buffer[0:]) != 77 || binary.LittleEndian.Uint64(buffer[8:]) != 5 ||
		binary.LittleEndian.Uint16(buffer[16:]) != 24 || buffer[18] != 4 || string(buffer[19:22]) != "abc" || buffer[22] != 0 || buffer[23] != 0 {
		t.Fatalf("record = % x", buffer[:24])
	}
	if buffer[24] != 0xff {
		t.Fatal("the encoder wrote past its record")
	}
	if size, _ := EncodeDirent(buffer[:], 1, 1, ramfs.KindFile, []byte("abcd")); size != 24 || buffer[18] != 8 {
		t.Fatalf("four-byte name: size=%d type=%d", size, buffer[18])
	}
	if size, _ := EncodeDirent(buffer[:], 1, 1, ramfs.KindFile, []byte("abcde")); size != 32 {
		t.Fatalf("five-byte name: size=%d", size)
	}
	if _, ok := EncodeDirent(buffer[:], 0, 1, ramfs.KindFile, []byte("x")); !ok || binary.LittleEndian.Uint64(buffer[0:]) != 1 {
		t.Fatal("a zero inode reached the record")
	}
}

func TestEncodeDirentRefusesWhatItCannotHold(t *testing.T) {
	var scratch [MaxDirentSize]byte
	longest := bytes.Repeat([]byte("n"), limits.NameMax)
	if size, ok := EncodeDirent(scratch[:], 1, 1, ramfs.KindFile, longest); !ok || size != MaxDirentSize || size%8 != 0 {
		t.Fatalf("longest name: size=%d ok=%v", size, ok)
	}
	for label, call := range map[string]func() (int, bool){
		"record one byte over the target": func() (int, bool) {
			return EncodeDirent(scratch[:23], 1, 1, ramfs.KindFile, []byte("abc"))
		},
		"empty name":       func() (int, bool) { return EncodeDirent(scratch[:], 1, 1, ramfs.KindFile, nil) },
		"name over limit":  func() (int, bool) { return EncodeDirent(scratch[:], 1, 1, ramfs.KindFile, append(longest, 'n')) },
		"name with NUL":    func() (int, bool) { return EncodeDirent(scratch[:], 1, 1, ramfs.KindFile, []byte("a\x00b")) },
		"name with slash":  func() (int, bool) { return EncodeDirent(scratch[:], 1, 1, ramfs.KindFile, []byte("a/b")) },
		"unknown kind":     func() (int, bool) { return EncodeDirent(scratch[:], 1, 1, 0, []byte("a")) },
		"no target at all": func() (int, bool) { return EncodeDirent(nil, 1, 1, ramfs.KindFile, []byte("a")) },
	} {
		if size, ok := call(); ok || size != 0 {
			t.Errorf("%s: size=%d ok=%v", label, size, ok)
		}
	}
}

// FuzzEncodeDirent packs records back to back and walks them the way a
// reader does: by d_reclen, never past the bytes the encoder reported.
func FuzzEncodeDirent(fuzz *testing.F) {
	fuzz.Add([]byte("a"), []byte("second-name"), uint16(64))
	fuzz.Add([]byte(strings.Repeat("n", 300)), []byte("x"), uint16(400))
	fuzz.Fuzz(func(t *testing.T, first, second []byte, capacity uint16) {
		buffer := make([]byte, int(capacity)+8)
		for index := range buffer {
			buffer[index] = 0xee
		}
		target := buffer[:capacity]
		used := 0
		for _, name := range [][]byte{first, second} {
			size, ok := EncodeDirent(target[used:], 9, uint64(used)+1, ramfs.KindFile, name)
			if !ok {
				if size != 0 {
					t.Fatalf("refused record reported %d bytes", size)
				}
				continue
			}
			record := target[used : used+size]
			if size%8 != 0 || size != DirentSize(len(name)) || int(binary.LittleEndian.Uint16(record[16:])) != size ||
				!bytes.Equal(record[19:19+len(name)], name) || record[19+len(name)] != 0 {
				t.Fatalf("record of %q = % x", name, record)
			}
			used += size
		}
		for index := used; index < len(buffer); index++ {
			if buffer[index] != 0xee {
				t.Fatalf("byte %d after the last record changed", index)
			}
		}
	})
}

func TestVectorPiece(t *testing.T) {
	wire := iovec([2]uint64{0x1000, 4}, [2]uint64{0x9999, 0}, [2]uint64{0x2000, 6})
	cases := []struct {
		position  uint64
		address   uint64
		remaining uint64
		ok        bool
	}{
		{0, 0x1000, 4, true},
		{3, 0x1003, 1, true},
		{4, 0x2000, 6, true}, // the empty element is skipped
		{9, 0x2005, 1, true},
		{10, 0, 0, false},
		{^uint64(0), 0, 0, false},
	}
	for _, test := range cases {
		address, remaining, ok := VectorPiece(wire, test.position)
		if address != test.address || remaining != test.remaining || ok != test.ok {
			t.Errorf("position %d = %#x,%d,%v", test.position, address, remaining, ok)
		}
	}
	if _, _, ok := VectorPiece(nil, 0); ok {
		t.Fatal("an empty vector has a piece")
	}
}

func TestClockOf(t *testing.T) {
	for id, want := range map[uint64]timekeeping.Clock{
		0: timekeeping.Realtime, 5: timekeeping.Realtime,
		1: timekeeping.Monotonic, 4: timekeeping.Monotonic, 6: timekeeping.Monotonic, 7: timekeeping.Monotonic,
	} {
		if clock, errno := ClockOf(id); clock != want || errno != 0 {
			t.Errorf("clock %d = %d errno=%d", id, clock, errno)
		}
	}
	// 2 and 3 are the CPU-time clocks; 11 is CLOCK_TAI.
	for _, id := range []uint64{2, 3, 8, 11, 1 << 40} {
		if _, errno := ClockOf(id); errno != abi.EINVAL {
			t.Errorf("clock %d: errno=%d", id, errno)
		}
	}
	var wire [TimespecSize]byte
	EncodeTimespec(&wire, timekeeping.Time{Seconds: -2, Nanoseconds: 999_999_999})
	if int64(binary.LittleEndian.Uint64(wire[0:])) != -2 || binary.LittleEndian.Uint64(wire[8:]) != 999_999_999 {
		t.Fatalf("timespec = % x", wire)
	}
}

func TestSleepTicks(t *testing.T) {
	const maximum = uint64(1) << 40
	cases := []struct {
		seconds, nanoseconds int64
		hz                   uint64
		want                 uint64
		errno                abi.Errno
	}{
		{0, 0, 100, 0, 0},
		{0, 1, 100, 2, 0},          // any part of a tick is a whole tick, plus the one in progress
		{0, 10_000_000, 100, 2, 0}, // exactly one tick
		{0, 10_000_001, 100, 3, 0},
		{0, 20_000_000, 100, 3, 0},
		{1, 0, 100, 101, 0},
		{1, 500_000_000, 1000, 1501, 0},
		{0, 999_999_999, 250, 251, 0},
		{-1, 0, 100, 0, abi.EINVAL},
		{0, -1, 100, 0, abi.EINVAL},
		{0, 1_000_000_000, 100, 0, abi.EINVAL},
		{1, 0, 0, 0, abi.EINVAL},
		{1 << 62, 0, 100, maximum, 0}, // no overflow; cut to the longest deadline
		{int64(maximum / 100), 999_999_999, 100, maximum, 0},
	}
	for _, test := range cases {
		var wire [TimespecSize]byte
		binary.LittleEndian.PutUint64(wire[0:], uint64(test.seconds))
		binary.LittleEndian.PutUint64(wire[8:], uint64(test.nanoseconds))
		if got, errno := SleepTicks(&wire, test.hz, maximum); got != test.want || errno != test.errno {
			t.Errorf("%d.%09d at %d Hz = %d errno=%d, want %d errno=%d", test.seconds, test.nanoseconds, test.hz, got, errno, test.want, test.errno)
		}
	}
}

func TestEncodeUname(t *testing.T) {
	var encoded [UnameSize]byte
	for index := range encoded {
		encoded[index] = 0xff
	}
	EncodeUname(&encoded, "0.2.0")
	want := []string{"Linux", "goos", "6.0.0-goos", "GoOS 0.2.0 Linux ABI", "x86_64", "(none)"}
	if UnameSize != 390 {
		t.Fatalf("struct utsname is %d bytes", UnameSize)
	}
	for index, value := range want {
		field := encoded[index*65 : (index+1)*65]
		end := bytes.IndexByte(field, 0)
		if end < 0 || string(field[:end]) != value || bytes.Trim(field[end:], "\x00") != nil && len(bytes.Trim(field[end:], "\x00")) != 0 {
			t.Errorf("field %d = %q", index, field)
		}
	}
	EncodeUname(&encoded, strings.Repeat("r", 200))
	if encoded[3*65+64] != 0 || encoded[4*65] != 'x' {
		t.Fatal("an overlong release ran into the next field")
	}
}

func TestResourceLimit(t *testing.T) {
	const stack = 8 << 20
	for resource, want := range map[uint64]uint64{
		abi.ResourceOpenFiles: LinuxMaxFD, abi.ResourceStack: stack, abi.ResourceData: BrkReserve,
		abi.ResourceCore: 0, abi.ResourceCPU: abi.ResourceInfinity, abi.ResourceFileSize: abi.ResourceInfinity, 15: abi.ResourceInfinity,
	} {
		if got, errno := ResourceLimit(resource, stack); got != want || errno != 0 {
			t.Errorf("resource %d = %d errno=%d", resource, got, errno)
		}
	}
	if _, errno := ResourceLimit(16, stack); errno != abi.EINVAL {
		t.Fatalf("resource 16: errno=%d", errno)
	}
}

func TestRandomFlags(t *testing.T) {
	for flags, want := range map[uint64]abi.Errno{
		0: 0, abi.RandomNonBlock: 0, abi.RandomRandom: 0, abi.RandomInsecure: 0,
		abi.RandomNonBlock | abi.RandomRandom: 0, abi.RandomNonBlock | abi.RandomInsecure: 0,
		abi.RandomRandom | abi.RandomInsecure: abi.EINVAL, 8: abi.EINVAL, ^uint64(0): abi.EINVAL,
	} {
		if errno := RandomFlags(flags); errno != want {
			t.Errorf("flags %#x: errno=%d, want %d", flags, errno, want)
		}
	}
}

func TestSleepNanosAndRemainingTicks(t *testing.T) {
	var wire [TimespecSize]byte
	binary.LittleEndian.PutUint64(wire[0:], 2)
	binary.LittleEndian.PutUint64(wire[8:], 500_000_000)
	if got := SleepNanos(&wire); got != 2_500_000_000 {
		t.Fatalf("2.5 s = %d ns", got)
	}
	binary.LittleEndian.PutUint64(wire[0:], ^uint64(0)>>1)
	if got := SleepNanos(&wire); got != ^uint64(0) {
		t.Fatalf("a huge sleep did not saturate: %d", got)
	}
	for _, test := range []struct{ nanos, want uint64 }{
		{0, 1}, {1, 1}, {9_999_999, 1}, {10_000_000, 1}, {10_000_001, 2}, {25_000_000, 3}, {1 << 62, 50},
	} {
		if got := RemainingTicks(test.nanos, 100, 50); got != test.want {
			t.Errorf("%d ns: %d ticks, want %d", test.nanos, got, test.want)
		}
	}
	if RemainingTicks(1, 0, 50) != 1 {
		t.Fatal("a zero tick rate must still wait one tick")
	}
}

func TestEncodeTimeval(t *testing.T) {
	var out [TimevalSize]byte
	EncodeTimeval(&out, timekeeping.Time{Seconds: 1790944000, Nanoseconds: 123456789})
	if sec, usec := binary.LittleEndian.Uint64(out[0:]), binary.LittleEndian.Uint64(out[8:]); sec != 1790944000 || usec != 123456 {
		t.Fatalf("timeval %d.%06d", sec, usec)
	}
}

func TestEncodeSysinfo(t *testing.T) {
	var out [SysinfoSize]byte
	for index := range out {
		out[index] = 0xff
	}
	EncodeSysinfo(&out, Sysinfo{UptimeSeconds: 42, TotalRAM: 1 << 30, FreeRAM: 1 << 29, Processes: 13})
	le := binary.LittleEndian
	if le.Uint64(out[0:]) != 42 || le.Uint64(out[32:]) != 1<<30 || le.Uint64(out[40:]) != 1<<29 ||
		le.Uint16(out[80:]) != 13 || le.Uint32(out[104:]) != 1 {
		t.Fatalf("fields wrong: % x", out)
	}
	for _, zero := range [][2]int{{8, 32}, {48, 80}, {82, 104}, {108, 112}} {
		for index := zero[0]; index < zero[1]; index++ {
			if out[index] != 0 {
				t.Fatalf("byte %d is %#x, want 0", index, out[index])
			}
		}
	}
}
