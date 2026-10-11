package admincmd

import (
	"bytes"
	"goos/internal/timekeeping"
	"strings"
	"testing"
)

func TestHexdumpRowsAndFailures(t *testing.T) {
	var out output
	if status := Hexdump(&input{}, &out); status != OK || out.Len() != 0 {
		t.Fatal("empty", status)
	}
	data := append([]byte("0123456789abcdef"), 0, 0x7f, 'A')
	if status := Hexdump(&input{data: data, chunk: 1}, &out); status != OK {
		t.Fatal(status)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "00000000  30 31 32 33") || !strings.HasSuffix(lines[0], "|0123456789abcdef|") || !strings.HasPrefix(lines[1], "00000010  00 7f 41") || !strings.HasSuffix(lines[1], "|..A|") {
		t.Fatal(out.String())
	}
	if Hexdump(&input{fail: true}, &out) != InputError {
		t.Fatal("read error")
	}
	out.fail = true
	if Hexdump(&input{data: data}, &out) != OutputError || Hexdump(&input{data: []byte("a")}, &out) != OutputError {
		t.Fatal("write error")
	}
	var row [80]byte
	if len(HexRow(0, bytes.Repeat([]byte{'a'}, 100), &row)) > len(row) {
		t.Fatal("row overflow")
	}
}

func TestUTCFormatAndBounds(t *testing.T) {
	for _, test := range []struct {
		seconds int64
		want    string
	}{
		{0, "1970-01-01T00:00:00Z\n"}, {-1, "1969-12-31T23:59:59Z\n"},
		{951782400, "2000-02-29T00:00:00Z\n"}, {-62167219200, "0000-01-01T00:00:00Z\n"},
		{253402300799, "9999-12-31T23:59:59Z\n"},
	} {
		var buffer [21]byte
		got, ok := UTC(timekeeping.Time{Seconds: test.seconds}, &buffer)
		if !ok || string(got) != test.want {
			t.Fatalf("%d = %q %v", test.seconds, got, ok)
		}
	}
	for _, value := range []timekeeping.Time{{Seconds: -62167219201}, {Seconds: 253402300800}, {Seconds: 1<<63 - 1}, {Seconds: -1 << 63}, {Nanoseconds: 1_000_000_000}} {
		var buffer [21]byte
		if _, ok := UTC(value, &buffer); ok {
			t.Fatal("out-of-range accepted", value)
		}
	}
}
