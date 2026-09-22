package logfmt

import (
	"strings"
	"testing"
)

func TestRoundTripAndFormat(t *testing.T) {
	fields, _ := AppendField(nil, "service", []byte("vfs"))
	fields, _ = AppendField(fields, "note", []byte("two words"))
	fields = append(fields, " pid="...)
	fields = AppendUint(fields, 7)
	var wire [MaxWire]byte
	size, ok := Encode(Info, []byte("init"), []byte("service started"), fields, wire[:])
	if !ok {
		t.Fatal("encode rejected a valid record")
	}
	record, ok := Decode(wire[:size])
	if !ok {
		t.Fatal("decode rejected encoded record")
	}
	record.Time, record.Process = 1_234_567_000, 1
	var text [MaxText]byte
	line := string(text[:Format(record, text[:])])
	want := "[    1.234567] INFO  init[1]: service started service=vfs note=\"two words\" pid=7\n"
	if line != want {
		t.Fatalf("got %q want %q", line, want)
	}
}

func TestRejectsMalformedWire(t *testing.T) {
	var wire [MaxWire]byte
	size, _ := Encode(Warn, []byte("vfs"), []byte("x"), nil, wire[:])
	cases := map[string][]byte{
		"short":     wire[:3],
		"truncated": wire[:size-1],
		"version":   append([]byte{9}, wire[1:size]...),
		"level":     append([]byte{Version, 9}, wire[2:size]...),
	}
	for name, data := range cases {
		if _, ok := Decode(data); ok {
			t.Fatalf("%s record accepted", name)
		}
	}
	bad := append([]byte(nil), wire[:size]...)
	bad[headerSize] = 'X'
	if _, ok := Decode(bad); ok {
		t.Fatal("upper-case component accepted")
	}
	bad = append([]byte(nil), wire[:size]...)
	bad[size-1] = '\n'
	if _, ok := Decode(bad); ok {
		t.Fatal("control character in message accepted")
	}
}

func TestBoundsOnEncodeAndFields(t *testing.T) {
	var wire [MaxWire]byte
	if _, ok := Encode(Info, []byte("x"), []byte(strings.Repeat("m", MaxMessage+1)), nil, wire[:]); ok {
		t.Fatal("oversized message accepted")
	}
	if _, ok := Encode(Info, []byte(strings.Repeat("c", MaxComponent+1)), []byte("m"), nil, wire[:]); ok {
		t.Fatal("oversized component accepted")
	}
	if _, ok := AppendField(nil, "k", []byte(`a"b`)); ok {
		t.Fatal("quote in value accepted")
	}
	fields := []byte(nil)
	for {
		next, ok := AppendField(fields, "key", []byte("value"))
		if !ok {
			break
		}
		fields = next
	}
	if len(fields) > MaxFields {
		t.Fatalf("fields grew to %d", len(fields))
	}
	var text [MaxText]byte
	record := Record{Time: ^uint64(0), Process: ^uint64(0), Level: Error, Component: []byte(strings.Repeat("c", MaxComponent)),
		Message: []byte(strings.Repeat("m", MaxMessage)), Fields: []byte(strings.Repeat("f", MaxFields))}
	if Format(record, text[:]) == 0 {
		t.Fatal("largest record does not fit MaxText")
	}
}

// TestFormatDoesNotAllocate guards the kernel, which leaks every allocation
// and cannot grow its heap after it leaves boot services.
func TestFormatDoesNotAllocate(t *testing.T) {
	record := Record{Time: 1234567890, Process: 7, Level: Warn, Component: []byte("kernel"), Message: []byte("launch failed"), Fields: []byte("status=2")}
	var target [MaxText]byte
	if allocations := testing.AllocsPerRun(100, func() { Format(record, target[:]) }); allocations != 0 {
		t.Fatalf("Format allocated %v times per run", allocations)
	}
	var fields [MaxFields]byte
	if allocations := testing.AllocsPerRun(100, func() { AppendUint(fields[:0], 987654321) }); allocations != 0 {
		t.Fatalf("AppendUint allocated %v times per run", allocations)
	}
}

func TestAppendFieldDoesNotAllocate(t *testing.T) {
	var fields [MaxFields]byte
	value := []byte("vfs")
	if allocations := testing.AllocsPerRun(100, func() { AppendField(fields[:0], "grant", value) }); allocations != 0 {
		t.Fatalf("AppendField allocated %v times per run", allocations)
	}
}
