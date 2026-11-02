package linux

import (
	"encoding/binary"
	"goos/internal/limits"
	"goos/kernel/ramfs"
	"strings"
	"testing"
)

func appendTestRecord(t *testing.T, batch []byte, id ramfs.NodeID, kind ramfs.Kind, next int, name string) []byte {
	t.Helper()
	stat := ramfs.Stat{ID: id, Kind: kind}
	var record [ramfs.MaxDirRecord]byte
	size := ramfs.AppendDirRecord(record[:], &stat, next, []byte(name))
	if size == 0 {
		t.Fatal("append")
	}
	return append(batch, record[:size]...)
}

// A getdents64 call turns each record into one linux_dirent64 whose d_off is
// the listing position, ending at the first that does not fit.
func TestBatchRecordsBecomeDirents(t *testing.T) {
	long := strings.Repeat("l", limits.NameMax)
	var batch []byte
	batch = appendTestRecord(t, batch, 7, ramfs.KindFile, 3, "first")
	batch = appendTestRecord(t, batch, 8, ramfs.KindDirectory, 9, "second")
	batch = appendTestRecord(t, batch, 9, ramfs.KindFile, 12, long)

	var target [MaxDirentSize]byte
	type want struct {
		inode, next uint64
		kind        byte
		name        string
	}
	for index, expected := range []want{{7, 3, 8, "first"}, {8, 9, 4, "second"}, {9, 12, 8, long}} {
		written, consumed, next, valid := EncodeBatchDirent(target[:], batch)
		if !valid || written != DirentSize(len(expected.name)) || next != expected.next {
			t.Fatalf("record %d: written %d next %d valid %v", index, written, next, valid)
		}
		if binary.LittleEndian.Uint64(target[0:]) != expected.inode || binary.LittleEndian.Uint64(target[8:]) != expected.next ||
			int(binary.LittleEndian.Uint16(target[16:])) != written || target[18] != expected.kind ||
			string(target[19:19+len(expected.name)]) != expected.name || target[19+len(expected.name)] != 0 {
			t.Fatalf("record %d: dirent % x", index, target[:32])
		}
		batch = batch[consumed:]
	}
	if len(batch) != 0 {
		t.Fatalf("%d bytes left", len(batch))
	}
}

func TestBatchRecordThatDoesNotFitStaysUnread(t *testing.T) {
	batch := appendTestRecord(t, nil, 7, ramfs.KindFile, 3, "first")
	var target [64]byte
	written, consumed, _, valid := EncodeBatchDirent(target[:DirentSize(5)-1], batch)
	if !valid || written != 0 || consumed != 0 {
		t.Fatalf("written %d consumed %d valid %v", written, consumed, valid)
	}
}

func TestMalformedBatchRecordIsRefused(t *testing.T) {
	batch := appendTestRecord(t, nil, 7, ramfs.KindFile, 3, "first")
	batch[54] = 0
	var target [64]byte
	if _, _, _, valid := EncodeBatchDirent(target[:], batch); valid {
		t.Fatal("accepted a record with no kind")
	}
	batch = appendTestRecord(t, nil, 7, ramfs.KindFile, 3, "a/b")
	if _, _, _, valid := EncodeBatchDirent(target[:], batch); valid {
		t.Fatal("accepted a name with a separator")
	}
}
