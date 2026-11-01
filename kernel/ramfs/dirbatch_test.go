package ramfs

import (
	"bytes"
	"testing"
)

func testDirStat() Stat {
	return Stat{ID: 9, Kind: KindDirectory, Size: 512, Created: Timestamp{Seconds: 10, Nanoseconds: 20, Available: true},
		Modified: Timestamp{Seconds: 30, Available: true}, Attributes: AttrReadOnly, AttributesAvailable: true, MountID: 77}
}

func TestDirRecordRoundTrip(t *testing.T) {
	for _, nameSize := range []int{1, 7, 8, MaxName} {
		name := bytes.Repeat([]byte("n"), nameSize)
		stat := testDirStat()
		target := make([]byte, MaxDirRecord)
		size := AppendDirRecord(target, &stat, 42, name)
		if size != DirRecordSize(nameSize) || size%dirRecordAlignment != 0 {
			t.Fatalf("name %d: size %d", nameSize, size)
		}
		var record DirRecord
		consumed, ok := record.Decode(target[:size])
		if !ok || consumed != size || record.Stat != stat || record.Next != 42 || !bytes.Equal(record.Name, name) {
			t.Fatalf("name %d: decoded %+v", nameSize, record)
		}
	}
}

// A record that does not fit or cannot be described writes nothing, so a batch
// never holds half an entry.
func TestAppendDirRecordRefuses(t *testing.T) {
	stat := testDirStat()
	target := make([]byte, MaxDirRecord)
	if AppendDirRecord(target[:DirRecordSize(4)-1], &stat, 1, []byte("name")) != 0 {
		t.Fatal("wrote past the target")
	}
	if AppendDirRecord(target, &stat, 1, nil) != 0 || AppendDirRecord(target, &stat, -1, []byte("n")) != 0 {
		t.Fatal("wrote an invalid record")
	}
	if AppendDirRecord(target, &stat, 1, make([]byte, MaxName+1)) != 0 {
		t.Fatal("wrote an overlong name")
	}
	stat.Kind = 0
	if AppendDirRecord(target, &stat, 1, []byte("n")) != 0 {
		t.Fatal("wrote an unknown kind")
	}
}

// The peer that wrote a batch is not trusted, so each field that could misdirect
// a decoder is checked.
func TestDirRecordDecodeRefusesDamage(t *testing.T) {
	stat := testDirStat()
	valid := make([]byte, 128)
	size := AppendDirRecord(valid, &stat, 5, []byte("name"))
	valid = valid[:size]
	damage := []func([]byte){
		func(b []byte) { b[52], b[53] = 0, 0 },       // no name
		func(b []byte) { b[52], b[53] = 0xff, 0xff }, // name past the limit
		func(b []byte) { b[52] = 200 },               // name past the batch
		func(b []byte) { b[54] = 0 },                 // no kind
		func(b []byte) { b[54] = 9 },                 // unknown kind
		func(b []byte) { b[55] = 0xff },              // unknown attributes
		func(b []byte) { b[56] = 0xf0 },              // unknown metadata
		func(b []byte) { b[60] = 1 },                 // reserved
		func(b []byte) { b[56], b[55] = 0, 1 },       // attributes marked absent
	}
	for index, apply := range damage {
		data := append([]byte(nil), valid...)
		apply(data)
		var record DirRecord
		if _, ok := record.Decode(data); ok {
			t.Fatalf("damage %d accepted", index)
		}
	}
	var record DirRecord
	if _, ok := record.Decode(valid[:dirRecordHeader-1]); ok {
		t.Fatal("short header accepted")
	}
}

func TestValidDirBatchChecksTheWholeRun(t *testing.T) {
	stat := testDirStat()
	data := make([]byte, 256)
	used := AppendDirRecord(data, &stat, 3, []byte("a"))
	used += AppendDirRecord(data[used:], &stat, 8, []byte("b"))
	if !ValidDirBatch(data[:used], 2, 0, 8) {
		t.Fatal("valid batch refused")
	}
	for _, bad := range []struct{ count, cursor, next int }{
		{1, 0, 8}, // a record the count leaves out
		{3, 0, 8}, // a record that is not there
		{2, 3, 8}, // a position that does not rise
		{2, 0, 9}, // a cursor that disagrees with the last record
	} {
		if ValidDirBatch(data[:used], bad.count, bad.cursor, bad.next) {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if !ValidDirBatch(nil, 0, 4, 4) {
		t.Fatal("an empty batch must keep its position")
	}
}
