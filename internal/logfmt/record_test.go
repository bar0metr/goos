package logfmt

import (
	"encoding/binary"
	"goos/internal/timekeeping"
	"strings"
	"testing"
)

func TestFullRecordPreservesKernelFieldsAndBounds(t *testing.T) {
	record := Record{Time: timekeeping.Time{Seconds: -123, Nanoseconds: 999999999}, Process: ^uint64(0), Level: Error, Component: []byte(strings.Repeat("c", MaxComponent)), Message: []byte(strings.Repeat("m", MaxMessage)), Fields: []byte("k=" + strings.Repeat("v", MaxFields-2))}
	var wire [FullWire]byte
	size, ok := EncodeRecord(record, wire[:])
	if !ok || size != FullWire {
		t.Fatalf("encode %d %v", size, ok)
	}
	got, ok := DecodeRecord(wire[:size])
	if !ok || got.Time != record.Time || got.Process != record.Process || got.Level != record.Level || string(got.Component) != string(record.Component) || string(got.Message) != string(record.Message) || string(got.Fields) != string(record.Fields) {
		t.Fatalf("decode %+v %v", got, ok)
	}
	wire[FullHeader+5] = 'x'
	if got.Component[0] != 'x' {
		t.Fatal("decoded fields do not borrow wire")
	}
	if _, ok := EncodeRecord(record, wire[:size-1]); ok {
		t.Fatal("short output accepted")
	}
	if _, ok := DecodeRecord(wire[:FullHeader-1]); ok {
		t.Fatal("short input accepted")
	}
	binary.LittleEndian.PutUint32(wire[8:12], timekeeping.NanosPerSecond)
	if _, ok := DecodeRecord(wire[:]); ok {
		t.Fatal("invalid timestamp accepted")
	}
	binary.LittleEndian.PutUint32(wire[8:12], 0)
	wire[12] = 1
	if _, ok := DecodeRecord(wire[:]); ok {
		t.Fatal("reserved bytes accepted")
	}
}

func TestFullRecordKeepsOldWriteEncoding(t *testing.T) {
	var full [FullWire]byte
	var old [MaxWire]byte
	record := Record{Level: Info, Component: []byte("logd"), Message: []byte("persisted"), Fields: []byte("n=1")}
	size, _ := EncodeRecord(record, full[:])
	oldSize, _ := Encode(record.Level, record.Component, record.Message, record.Fields, old[:])
	if string(full[FullHeader:size]) != string(old[:oldSize]) {
		t.Fatal("old write encoding changed")
	}
}
