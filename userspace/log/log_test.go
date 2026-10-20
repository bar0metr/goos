package log

import (
	"fmt"
	"goos/internal/logfmt"
	"strconv"
	"sync"
	"testing"
)

// TestEncodeConcurrentRecordsIntact encodes events from several goroutines
// at once. Each one's record decodes to exactly its own message and fields:
// nothing is shared between calls.
func TestEncodeConcurrentRecordsIntact(t *testing.T) {
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					for round := 0; round < 500; round++ {
						var wire [logfmt.MaxWire]byte
						message := "event" + strconv.Itoa(worker)
						size, ok := encode(logfmt.Warn, "svc", message, []Field{
							Uint("round", uint64(round)),
							Str("worker", strconv.Itoa(worker)),
						}, &wire)
						if !ok {
							t.Error("encode failed")
							return
						}
						record, ok := logfmt.Decode(wire[:size])
						want := "round=" + strconv.Itoa(round) + " worker=" + strconv.Itoa(worker)
						if !ok || string(record.Message) != message || string(record.Component) != "svc" || string(record.Fields) != want {
							t.Errorf("record %q %q, want %q %q", record.Message, record.Fields, message, want)
						}
					}
				}(worker)
			}
			group.Wait()
		})
	}
}

func TestEncodeRefusesOversizedFields(t *testing.T) {
	var wire [logfmt.MaxWire]byte
	long := make([]byte, logfmt.MaxFields)
	for index := range long {
		long[index] = 'x'
	}
	if _, ok := encode(logfmt.Info, "svc", "m", []Field{Bytes("k", long)}, &wire); ok {
		t.Fatal("encode accepted fields longer than a record holds")
	}
}
