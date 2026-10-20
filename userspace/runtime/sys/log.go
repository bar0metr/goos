package sys

import (
	"encoding/binary"
	"goos/internal/logfmt"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"unsafe"
)

// LogBatch borrows a fixed snapshot from caller-owned storage. First is the
// next cursor after an overwrite; records have sequences First..First+Count-1.
// Total is the snapshot's end cursor, not a live index into the current ring.
type LogBatch struct {
	Oldest, Total, First uint64
	Count                int
	Gap                  bool
	wire                 []byte
}

func (batch LogBatch) Record(index int) (logfmt.Record, bool) {
	if index < 0 || index >= batch.Count || index >= kcall.LogReadMax || len(batch.wire) != kcall.LogReadSize {
		return logfmt.Record{}, false
	}
	offset := kcall.LogReadHeader + index*kcall.LogReadRecordSize
	size := int(binary.LittleEndian.Uint16(batch.wire[offset : offset+2]))
	if size > logfmt.FullWire || size < logfmt.FullHeader {
		return logfmt.Record{}, false
	}
	return logfmt.DecodeRecord(batch.wire[offset+2 : offset+2+size])
}

func DecodeLogBatch(data []byte, cursor uint64) (LogBatch, bool) {
	if len(data) != kcall.LogReadSize {
		return LogBatch{}, false
	}
	batch := LogBatch{Oldest: binary.LittleEndian.Uint64(data[0:8]), Total: binary.LittleEndian.Uint64(data[8:16]), First: binary.LittleEndian.Uint64(data[16:24]), Count: int(binary.LittleEndian.Uint32(data[24:28])), Gap: binary.LittleEndian.Uint32(data[28:32]) == 1, wire: data}
	if batch.Total-batch.Oldest > kcall.LogReadMax || batch.Count < 0 || batch.Count > kcall.LogReadMax || binary.LittleEndian.Uint32(data[28:32]) > 1 {
		return LogBatch{}, false
	}
	first := cursor
	if logfmt.SequenceBefore(first, batch.Oldest) {
		first = batch.Oldest
	}
	count := uint64(0)
	if logfmt.SequenceBefore(first, batch.Total) {
		count = batch.Total - first
	}
	if batch.First != first || uint64(batch.Count) != count || batch.Gap != logfmt.SequenceBefore(cursor, batch.Oldest) {
		return LogBatch{}, false
	}
	for index := 0; index < batch.Count; index++ {
		if _, ok := batch.Record(index); !ok {
			return LogBatch{}, false
		}
	}
	return batch, true
}

// LogReadSequence reads up to 64 records atomically. Storage must remain live
// until the returned batch is no longer used; large storage belongs in BSS.
func LogReadSequence(device handle.ID, cursor uint64, target *[kcall.LogReadSize]byte) (LogBatch, kcall.Status) {
	if target == nil {
		return LogBatch{}, kcall.StatusInvalidArgument
	}
	touch(target[:])
	result := status(logReadSequence(uint64(device), cursor, unsafe.Pointer(&target[0])))
	if result != kcall.StatusOK {
		return LogBatch{}, result
	}
	batch, valid := DecodeLogBatch(target[:], cursor)
	if !valid {
		return LogBatch{}, kcall.StatusCorruptData
	}
	return batch, result
}

// CursorInterest extends the original interest with a full-width next-record
// sequence. Cursor must be zero for non-log objects. Existing Interest/WaitAny
// layouts and behavior are unchanged.
type CursorInterest struct {
	Handle handle.ID
	Events kcall.WaitEvent
	Cursor uint64
}

func WaitAnyCursor(interests *[kcall.WaitAnyMaxObjects]CursorInterest, count int, timeout uint64) (uint64, kcall.Status) {
	w := current()
	if interests == nil || count < 0 || count > kcall.WaitAnyMaxObjects {
		return 0, kcall.StatusInvalidArgument
	}
	w.waitWire[0], w.waitWire[1] = timeout, 0
	touch(unsafe.Slice((*byte)(unsafe.Pointer(&interests[0])), count*int(kcall.WaitAnyCursorInterestSize)))
	result := status(waitAny(unsafe.Pointer(&interests[0]), uintptr(uint64(count)|kcall.WaitAnyCursorFlag), unsafe.Pointer(&w.waitWire[0])))
	return w.waitWire[1], result
}
