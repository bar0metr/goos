package admincmd

import (
	"bytes"
	"strings"
	"testing"
)

type input struct {
	data            []byte
	position, chunk int
	fail            bool
	replacement     []byte
	rewindFail      bool
}

func (in *input) Read(out []byte) (int, Status) {
	if in.fail {
		return 0, InputError
	}
	if in.position == len(in.data) {
		return 0, EOF
	}
	size := len(out)
	if in.chunk > 0 && size > in.chunk {
		size = in.chunk
	}
	n := copy(out[:size], in.data[in.position:])
	in.position += n
	return n, OK
}
func (in *input) Rewind() Status {
	if in.rewindFail {
		return InputError
	}
	in.position = 0
	if in.replacement != nil {
		in.data = in.replacement
	}
	return OK
}

type output struct {
	bytes.Buffer
	fail bool
}

func (out *output) Write(data []byte) Status {
	if out.fail {
		return OutputError
	}
	out.Buffer.Write(data)
	return OK
}

func TestHeadTailLogicalLinesAndChunks(t *testing.T) {
	for _, data := range []string{"", "a", "a\n", "a\nb", "a\nb\n", "\n\n\n", strings.Repeat("x", 20000) + "\ny\nlast"} {
		for _, count := range []uint64{0, 1, 2, 10, ^uint64(0)} {
			for _, chunk := range []int{1, 3, 256} {
				lines := strings.SplitAfter(data, "\n")
				if lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
				n := len(lines)
				if count < uint64(n) {
					n = int(count)
				}
				in := &input{data: []byte(data), chunk: chunk}
				var out output
				if status := Head(in, &out, count); status != OK || out.String() != strings.Join(lines[:n], "") {
					t.Fatalf("head %q count %d chunk %d: %q %v", data, count, chunk, out.String(), status)
				}
				in.position = 0
				out.Reset()
				if status := Tail(in, &out, count); status != OK || out.String() != strings.Join(lines[len(lines)-n:], "") {
					t.Fatalf("tail count %d chunk %d: %q %v", count, chunk, out.String(), status)
				}
			}
		}
	}
}

func TestTailMutationAndFailures(t *testing.T) {
	for _, replacement := range []string{"a\n", "a\nb\nc", "a-b\n", "a\nb\n"} {
		in := &input{data: []byte("a\nb"), replacement: []byte(replacement)}
		var out output
		if status := Tail(in, &out, 1); status != ChangedError {
			t.Fatalf("mutation %q = %v", replacement, status)
		}
	}
	var out output
	if Tail(&input{data: []byte("a"), rewindFail: true}, &out, 1) != InputError {
		t.Fatal("rewind failure")
	}
	if Head(&input{fail: true}, &out, 1) != InputError || Tail(&input{fail: true}, &out, 1) != InputError {
		t.Fatal("read failure")
	}
	out.fail = true
	if Head(&input{data: []byte("a")}, &out, 1) != OutputError || Tail(&input{data: []byte("a")}, &out, 1) != OutputError {
		t.Fatal("write failure")
	}
}

func TestGrepLiteralCaseAndBounds(t *testing.T) {
	for _, test := range []struct {
		data, pattern, want string
		fold, match         bool
	}{
		{"", "", "", false, false}, {"\n", "", "\n", false, true},
		{"abc\nXAbC\nlast abc", "abc", "abc\nlast abc", false, true},
		{"abc\nXAbC\nlast abc", "ABC", "abc\nXAbC\nlast abc", true, true},
		{"ab.c\nabc\n", ".", "ab.c\n", false, true},
		{"abc", "z", "", false, false}, {"É\né\n", "é", "é\n", true, true},
		{strings.Repeat("x", MaxLine), "x", strings.Repeat("x", MaxLine), false, true},
	} {
		for _, chunk := range []int{1, 256} {
			var out output
			matched, status := Grep(&input{data: []byte(test.data), chunk: chunk}, &out, []byte(test.pattern), test.fold)
			if status != OK || matched != test.match || out.String() != test.want {
				t.Fatalf("grep %#v = %v %v %q", test, matched, status, out.String())
			}
		}
	}
	var out output
	if _, status := Grep(&input{}, &out, make([]byte, MaxPattern+1), false); status != LimitError {
		t.Fatal("pattern bound")
	}
	if _, status := Grep(&input{data: bytes.Repeat([]byte{'x'}, MaxLine+1)}, &out, []byte("z"), false); status != LimitError {
		t.Fatal("line bound")
	}
	if _, status := Grep(&input{fail: true}, &out, nil, false); status != InputError {
		t.Fatal("read failure")
	}
	out.fail = true
	if _, status := Grep(&input{data: []byte("a\n")}, &out, nil, false); status != OutputError {
		t.Fatal("write failure")
	}
	if _, status := Grep(&input{data: []byte("a")}, &out, nil, false); status != OutputError {
		t.Fatal("final write failure")
	}
}

func TestWCStreaming(t *testing.T) {
	for _, test := range []struct {
		text string
		want Counts
	}{
		{"", Counts{}}, {"a", Counts{0, 1, 1}}, {"a b\n", Counts{1, 2, 4}},
		{" \t\n\r\v\f", Counts{1, 0, 6}}, {"one\ntwo three", Counts{1, 3, 13}},
		{strings.Repeat("a", 20000), Counts{0, 1, 20000}},
	} {
		for _, chunk := range []int{1, 256} {
			got, status := WC(&input{data: []byte(test.text), chunk: chunk})
			if status != OK || got != test.want {
				t.Fatalf("wc %q = %+v %v", test.text, got, status)
			}
		}
	}
	if _, status := WC(&input{fail: true}); status != InputError {
		t.Fatal("read failure")
	}
}

type badInput int

func (in badInput) Read(out []byte) (int, Status) { return int(in), OK }
func TestReadRejectsMalformedAndOversizedInput(t *testing.T) {
	for _, size := range []int{-1, 0, ChunkSize + 1} {
		if _, status := WC(badInput(size)); status != InputError {
			t.Fatal("invalid read size", size)
		}
	}
	total := MaxInput
	var buffer [ChunkSize]byte
	if _, status := read(&input{data: []byte("x")}, buffer[:], &total); status != LimitError {
		t.Fatal("input bound")
	}
}

func TestCountUint64AndFormatting(t *testing.T) {
	for _, text := range []string{"0", "10", "18446744073709551615"} {
		if _, ok := Count([]byte(text)); !ok {
			t.Fatal(text)
		}
	}
	for _, text := range []string{"", "-1", "+1", "1x", "18446744073709551616", "000000000000000000000"} {
		if _, ok := Count([]byte(text)); ok {
			t.Fatal(text)
		}
	}
	if string(AppendUint(nil, ^uint64(0))) != "18446744073709551615" {
		t.Fatal("format")
	}
}
