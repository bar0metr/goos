package sys

import (
	"encoding/binary"
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"testing"
	"unsafe"
)

func testLogWire(wire *[kcall.LogReadSize]byte, oldest, total, cursor uint64) {
	clear(wire[:])
	first := cursor
	if logfmt.SequenceBefore(first, oldest) {
		first = oldest
		binary.LittleEndian.PutUint32(wire[28:32], 1)
	}
	binary.LittleEndian.PutUint64(wire[0:8], oldest)
	binary.LittleEndian.PutUint64(wire[8:16], total)
	binary.LittleEndian.PutUint64(wire[16:24], first)
	if !logfmt.SequenceBefore(first, total) {
		return
	}
	binary.LittleEndian.PutUint32(wire[24:28], uint32(total-first))
	for index := 0; uint64(index) < total-first; index++ {
		offset := kcall.LogReadHeader + index*kcall.LogReadRecordSize
		size, _ := logfmt.EncodeRecord(logfmt.Record{Time: timekeeping.Time{Seconds: int64(first + uint64(index))}, Process: 42, Level: logfmt.Info, Component: []byte("test"), Message: []byte("message"), Fields: []byte("a=1")}, wire[offset+2:offset+kcall.LogReadRecordSize])
		binary.LittleEndian.PutUint16(wire[offset:offset+2], uint16(size))
	}
}

func TestStructuredBatchBoundsGapAndBorrowedRecords(t *testing.T) {
	var wire [kcall.LogReadSize]byte
	for _, cursor := range []uint64{0, 10, 13, 14, 15} {
		testLogWire(&wire, 10, 14, cursor)
		batch, valid := DecodeLogBatch(wire[:], cursor)
		if !valid || batch.Gap != (cursor < 10) {
			t.Fatalf("cursor %d batch %+v valid %v", cursor, batch, valid)
		}
		for i := 0; i < batch.Count; i++ {
			record, ok := batch.Record(i)
			if !ok || record.Time.Seconds != int64(batch.First+uint64(i)) || record.Process != 42 || string(record.Fields) != "a=1" {
				t.Fatalf("record %+v valid %v", record, ok)
			}
		}
		if _, ok := batch.Record(batch.Count); ok {
			t.Fatal("out of range record")
		}
	}
	testLogWire(&wire, 0, 1, 0)
	batch, _ := DecodeLogBatch(wire[:], 0)
	record, _ := batch.Record(0)
	wire[kcall.LogReadHeader+2+logfmt.FullHeader+5] = 'x'
	if record.Component[0] != 'x' {
		t.Fatal("batch unexpectedly copied fields")
	}
}

func TestStructuredBatchRejectsTornOrCorruptWires(t *testing.T) {
	for _, mutate := range []func([]byte){
		func(w []byte) { binary.LittleEndian.PutUint64(w[:8], 6) },
		func(w []byte) { binary.LittleEndian.PutUint64(w[8:16], 100) },
		func(w []byte) { binary.LittleEndian.PutUint64(w[16:24], 3) },
		func(w []byte) { binary.LittleEndian.PutUint32(w[24:28], 65) },
		func(w []byte) { w[28] = 2 },
		func(w []byte) { binary.LittleEndian.PutUint16(w[kcall.LogReadHeader:], 300) },
		func(w []byte) { w[kcall.LogReadHeader+2+logfmt.FullHeader] = 99 },
	} {
		var wire [kcall.LogReadSize]byte
		testLogWire(&wire, 0, 5, 0)
		mutate(wire[:])
		if _, valid := DecodeLogBatch(wire[:], 0); valid {
			t.Fatal("corrupt batch accepted")
		}
	}
	if _, valid := DecodeLogBatch(nil, 0); valid {
		t.Fatal("empty batch wire accepted")
	}
	if _, valid := (LogBatch{Count: 1}).Record(0); valid {
		t.Fatal("unbacked batch record")
	}
}

func TestLogAndCursorWrappersKeepExistingABI(t *testing.T) {
	var interest Interest
	var cursor CursorInterest
	if unsafe.Sizeof(interest) != 16 || kcall.WaitAnyInterestSize != 16 || kcall.WaitAnyResultSize != 16 {
		t.Fatal("existing WaitAny ABI changed")
	}
	if unsafe.Sizeof(cursor) != uintptr(kcall.WaitAnyCursorInterestSize) || unsafe.Offsetof(cursor.Events) != 8 || unsafe.Offsetof(cursor.Cursor) != 16 {
		t.Fatal("cursor interest ABI")
	}
	if kcall.LogReadRecordSize < logfmt.FullWire+2 || kcall.OperationLogRead != 26 || kcall.OperationLogBounds != 33 {
		t.Fatal("log ABI changed")
	}
	if _, status := WaitAnyCursor(nil, 1, 0); status != kcall.StatusInvalidArgument {
		t.Fatal("nil interests")
	}
	var interests [kcall.WaitAnyMaxObjects]CursorInterest
	for _, count := range []int{-1, kcall.WaitAnyMaxObjects + 1} {
		if _, status := WaitAnyCursor(&interests, count, 0); status != kcall.StatusInvalidArgument {
			t.Fatal("invalid count")
		}
	}
	if _, status := WaitAnyCursor(&interests, 1, 0); status != kcall.StatusUnsupported {
		t.Fatal("host cursor wait status")
	}
	if _, status := LogReadSequence(handle.ID(1), 0, nil); status != kcall.StatusInvalidArgument {
		t.Fatal("nil snapshot")
	}
	var wire [kcall.LogReadSize]byte
	if _, status := LogReadSequence(handle.ID(1), 0, &wire); status != kcall.StatusUnsupported {
		t.Fatal("host log status")
	}
	testLogWire(&wire, 0, 5, 0)
	if allocations := testing.AllocsPerRun(100, func() { DecodeLogBatch(wire[:], 0) }); allocations != 0 {
		t.Fatalf("batch allocations %v", allocations)
	}
}

func TestStructuredBatchAcrossSequenceCounterWrap(t *testing.T) {
	var wire [kcall.LogReadSize]byte
	oldest, total := ^uint64(0)-3, uint64(2)
	for _, cursor := range []uint64{oldest, ^uint64(0) - 1, 0, 2, 3, oldest - 3} {
		testLogWire(&wire, oldest, total, cursor)
		batch, valid := DecodeLogBatch(wire[:], cursor)
		if !valid || batch.Gap != logfmt.SequenceBefore(cursor, oldest) {
			t.Fatalf("wrapped cursor%d batch%+v valid%v", cursor, batch, valid)
		}
		for i := 0; i < batch.Count; i++ {
			if record, ok := batch.Record(i); !ok || record.Time.Seconds != int64(batch.First+uint64(i)) {
				t.Fatal("wrapped record sequence")
			}
		}
	}
}
