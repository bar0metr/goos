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
		{65, LinuxMaxFD, 0},
		{1024, LinuxMaxFD, 0},
		{0x7fffffff, LinuxMaxFD, 0},
		{^uint64(0), 0, abi.EINVAL},
		{0x80000000, 0, abi.EINVAL},
		// Only the low 32 bits are the C int Linux reads.
		{0x1_0000_0003, 3, 0},
	} {
		got, errno := SelectCount(test.argument)
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
	if FDSetBytes(LinuxMaxFD) > FDSetSize {
		t.Fatalf("a full table needs %d bytes, more than %d", FDSetBytes(LinuxMaxFD), FDSetSize)
	}
}

func TestFDSetRoundTrip(t *testing.T) {
	var wire [FDSetSize]byte
	binary.LittleEndian.PutUint64(wire[:], 1<<0|1<<3|1<<9|1<<63)
	set := DecodeFDSet(wire[:], 10)
	if !set.Has(0) || !set.Has(3) || !set.Has(9) || set.Has(63) || set.Has(1) {
		t.Fatalf("decoded set %#x", set)
	}
	if set.Has(-1) || set.Has(LinuxMaxFD) {
		t.Fatal("out-of-range descriptors are members")
	}
	full := DecodeFDSet(wire[:], 64)
	if !full.Has(63) {
		t.Fatal("descriptor 63 lost at count 64")
	}
	if empty := DecodeFDSet(wire[:], 0); empty != (FDSet{}) {
		t.Fatalf("count 0 decoded %#x", empty)
	}
	if short := DecodeFDSet(nil, 64); short != (FDSet{}) {
		t.Fatalf("empty wire decoded %#x", short)
	}

	var result FDSet
	result.Add(3)
	result.Add(-1)
	result.Add(LinuxMaxFD)
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
		set := DecodeFDSet(wire, count)
		for fd := 0; fd < LinuxMaxFD; fd++ {
			if set.Has(fd) && (fd >= count || fd/8 >= len(wire) || wire[fd/8]&(1<<(fd%8)) == 0) {
				t.Fatalf("descriptor %d is a member of %#x from %x at count %d", fd, set, wire, count)
			}
		}
	})
}
