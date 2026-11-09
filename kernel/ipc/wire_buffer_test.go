package ipc

import (
	"encoding/binary"
	"testing"
)

func TestDecodeBufferBounds(t *testing.T) {
	original := message(KindRequest, 77)
	var decoded Message
	var encoded [WireMaxSize]byte
	if _, ok := EncodeFrom(&original, encoded[:]); !ok || !DecodeBuffer(encoded[:], &decoded) || decoded.Correlation != original.Correlation {
		t.Fatal("valid padded buffer rejected")
	}
	var result Message
	for _, size := range []int{0, WireHeaderSize - 1} {
		if DecodeBuffer(make([]byte, size), &result) {
			t.Fatalf("accepted %d-byte buffer", size)
		}
	}
	for _, tc := range []struct {
		payload uint16
		handles byte
	}{
		{MaxPayload + 1, 0},
		{0, MaxAttachedHandles + 1},
		{0xffff, 0xff},
	} {
		var data [WireMaxSize]byte
		binary.LittleEndian.PutUint16(data[24:], tc.payload)
		data[26] = tc.handles
		if DecodeBuffer(data[:], &result) {
			t.Fatalf("accepted payload %d handles %d", tc.payload, tc.handles)
		}
	}
	data := make([]byte, WireHeaderSize)
	binary.LittleEndian.PutUint16(data[24:], 1)
	if DecodeBuffer(data, &result) {
		t.Fatal("accepted truncated payload")
	}
}
