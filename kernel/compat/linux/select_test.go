package linux

import (
	"encoding/binary"
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestSelectCount(t *testing.T) {
	for _, test := range []struct {
		argument uint64
		want     int
		errno    abi.Errno
	}{
		{0, 0, 0},
		{1, 1, 0},
		{64, 64, 0},
		{65, 65, 0},
		{1024, 1024, 0},
		{1025, 1025, 0},
		{2000, 1088, 0},
		{0x7fffffff, 1088, 0},
		{^uint64(0), 0, abi.EINVAL},
		{0x80000000, 0, abi.EINVAL},
		// Only the low 32 bits are the C int Linux reads.
		{0x1_0000_0003, 3, 0},
	} {
		// 1088 is the capacity of a table that grew one page.
		got, errno := SelectCount(test.argument, 1088)
		if got != test.want || errno != test.errno {
			t.Fatalf("SelectCount(%#x) = %d, %d; want %d, %d", test.argument, got, errno, test.want, test.errno)
		}
	}
}

func TestFDSetBytes(t *testing.T) {
	for count, want := range map[int]int{0: 0, 1: 8, 63: 8, 64: 8, 65: 16} {
		if got := FDSetBytes(count); got != want {
			t.Fatalf("FDSetBytes(%d) = %d, want %d", count, got, want)
		}
	}
	if FDSetBytes(FDSetWindow) != FDSetSize {
		t.Fatalf("a window needs %d bytes, not %d", FDSetBytes(FDSetWindow), FDSetSize)
	}
}

func TestFDSetRoundTrip(t *testing.T) {
	var wire [FDSetSize]byte
	binary.LittleEndian.PutUint64(wire[:], 1<<0|1<<3|1<<9|1<<63)
	var set FDSet
	DecodeFDSet(wire[:], 10, &set)
	if !set.Has(0) || !set.Has(3) || !set.Has(9) || set.Has(63) || set.Has(1) {
		t.Fatalf("decoded set %#x", set)
	}
	if set.Has(-1) || set.Has(FDSetWindow) {
		t.Fatal("out-of-range descriptors are members")
	}
	var full FDSet
	DecodeFDSet(wire[:], 64, &full)
	if !full.Has(63) {
		t.Fatal("descriptor 63 lost at count 64")
	}
	if DecodeFDSet(wire[:], 0, &full); full != (FDSet{}) {
		t.Fatalf("count 0 decoded %#x", full)
	}
	DecodeFDSet(wire[:], 64, &full)
	if DecodeFDSet(nil, 64, &full); full != (FDSet{}) {
		t.Fatalf("empty wire decoded %#x", full)
	}

	var result FDSet
	result.Add(3)
	result.Add(-1)
	result.Add(FDSetWindow)
	for index := range wire {
		wire[index] = 0xff
	}
	EncodeFDSet(wire[:], &result)
	if got := binary.LittleEndian.Uint64(wire[:]); got != 1<<3 {
		t.Fatalf("encoded set %#x", got)
	}
}

func TestSelectEvents(t *testing.T) {
	if got := SelectEvents(false, false); got != PollPriority {
		t.Fatalf("no sets asks for %#x", got)
	}
	if got := SelectEvents(true, false); got != PollPriority|PollIn|PollReadNormal|PollHangup|PollError {
		t.Fatalf("read set asks for %#x", got)
	}
	if got := SelectEvents(false, true); got != PollPriority|PollOut|PollWriteNormal|PollError {
		t.Fatalf("write set asks for %#x", got)
	}
	// A hung-up pipe is readable, an erroring one is both.
	hangup := PollRevents(SelectEvents(true, true), PollReadiness{Hangup: true})
	if hangup&SelectReadEvents == 0 || hangup&SelectWriteEvents != 0 {
		t.Fatalf("hangup reports %#x", hangup)
	}
	failed := PollRevents(SelectEvents(true, true), PollReadiness{Error: true})
	if failed&SelectReadEvents == 0 || failed&SelectWriteEvents == 0 || failed&SelectExceptEvents != 0 {
		t.Fatalf("error reports %#x", failed)
	}
}

func timeval(seconds, micros int64) *[TimevalSize]byte {
	var wire [TimevalSize]byte
	binary.LittleEndian.PutUint64(wire[0:], uint64(seconds))
	binary.LittleEndian.PutUint64(wire[8:], uint64(micros))
	return &wire
}

func TestSelectTimevalNanos(t *testing.T) {
	for _, test := range []struct {
		seconds, micros int64
		want            uint64
		errno           abi.Errno
	}{
		{0, 0, 0, 0},
		{0, 1, 1000, 0},
		{1, 500000, 1500000000, 0},
		{0, 1500000, 1500000000, 0},
		{0, 999999, 999999000, 0},
		{1, -1000000, 0, 0},
		{2, -1500000, 0, abi.EINVAL},
		{-1, 0, 0, abi.EINVAL},
		{0, -1, 0, abi.EINVAL},
		{1 << 62, 0, ^uint64(0), 0},
	} {
		got, errno := SelectTimevalNanos(timeval(test.seconds, test.micros))
		if got != test.want || errno != test.errno {
			t.Fatalf("timeval {%d, %d} = %d, %d; want %d, %d", test.seconds, test.micros, got, errno, test.want, test.errno)
		}
	}
}

func TestEncodeRemaining(t *testing.T) {
	var wire [TimevalSize]byte
	EncodeRemainingTimeval(&wire, 2_345_678_999)
	if binary.LittleEndian.Uint64(wire[0:]) != 2 || binary.LittleEndian.Uint64(wire[8:]) != 345678 {
		t.Fatalf("timeval %x", wire)
	}
	var spec [TimespecSize]byte
	EncodeRemainingTimespec(&spec, 2_345_678_999)
	if binary.LittleEndian.Uint64(spec[0:]) != 2 || binary.LittleEndian.Uint64(spec[8:]) != 345678999 {
		t.Fatalf("timespec %x", spec)
	}
}

func TestDecodeSelectArguments(t *testing.T) {
	var wire [SelectArgumentsSize]byte
	binary.LittleEndian.PutUint64(wire[0:], 0x7000_1000)
	binary.LittleEndian.PutUint64(wire[8:], SignalSetSize)
	if address, size := DecodeSelectArguments(&wire); address != 0x7000_1000 || size != SignalSetSize {
		t.Fatalf("arguments %#x, %d", address, size)
	}
}

func FuzzDecodeFDSet(f *testing.F) {
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 17)
	f.Fuzz(func(t *testing.T, wire []byte, count int) {
		var set FDSet
		DecodeFDSet(wire, count, &set)
		for fd := 0; fd < FDSetWindow; fd++ {
			if set.Has(fd) && (fd >= count || fd/8 >= len(wire) || wire[fd/8]&(1<<(fd%8)) == 0) {
				t.Fatalf("descriptor %d is a member of %#x from %x at count %d", fd, set, wire, count)
			}
		}
	})
}

// TestSelectWindowsCoverLargeSets walks a 1024-descriptor fd_set the way the
// kernel's select does, one window at a time, and checks that descriptors in
// the second window and the last one below nfds take part.
func TestSelectWindowsCoverLargeSets(t *testing.T) {
	const nfds = 1024
	user := make([]byte, FDSetBytes(nfds))
	for _, fd := range []int{0, 511, 512, 1000, 1023} {
		user[fd/8] |= 1 << (fd % 8)
	}
	count, errno := SelectCount(nfds, 64+FDChunkEntries)
	if errno != 0 || count != nfds {
		t.Fatalf("SelectCount = %d errno=%d", count, errno)
	}
	var in, out FDSet
	var seen []int
	for base := 0; base < count; base += FDSetWindow {
		size := FDSetBytes(min(FDSetWindow, count-base))
		wire := user[base/8 : base/8+size]
		DecodeFDSet(wire, count-base, &in)
		out = FDSet{}
		for bit := 0; bit < FDSetWindow && base+bit < count; bit++ {
			if in.Has(bit) {
				seen = append(seen, base+bit)
				if (base+bit)%2 == 0 {
					out.Add(bit)
				}
			}
		}
		EncodeFDSet(wire, &out)
	}
	if len(seen) != 5 || seen[2] != 512 || seen[3] != 1000 || seen[4] != 1023 {
		t.Fatalf("select saw %v", seen)
	}
	for fd := 0; fd < nfds; fd++ {
		member := user[fd/8]&(1<<(fd%8)) != 0
		if want := fd == 0 || fd == 512 || fd == 1000; member != want {
			t.Fatalf("result bit %d = %v", fd, member)
		}
	}
	// nfds past the table's capacity is cut to it.
	if count, _ := SelectCount(4097, 64); count != 64 {
		t.Fatalf("count cut to %d", count)
	}
}
