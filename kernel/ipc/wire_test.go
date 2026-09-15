package ipc

import "testing"

func TestWireRoundTrip(t *testing.T) {
	original := message(KindRequest, 77)
	var storage [WireMaxSize]byte
	size, ok := Encode(original, storage[:])
	if !ok {
		t.Fatal("encode failed")
	}
	decoded, ok := Decode(storage[:size])
	if !ok || decoded.ProtocolID != original.ProtocolID || decoded.Version != original.Version ||
		decoded.Kind != original.Kind || decoded.Correlation != original.Correlation || decoded.PayloadSize != original.PayloadSize ||
		string(decoded.Payload[:decoded.PayloadSize]) != "ping" {
		t.Fatalf("unexpected decoded message: %#v", decoded)
	}
}

func TestWireRejectsMalformedInput(t *testing.T) {
	original := message(KindNotification, 0)
	var storage [WireMaxSize]byte
	size, _ := Encode(original, storage[:])
	tests := [][]byte{
		storage[:WireHeaderSize-1],
		append([]byte(nil), storage[:size-1]...),
		append(append([]byte(nil), storage[:size]...), 0),
	}
	badMagic := append([]byte(nil), storage[:size]...)
	badMagic[0] = 0
	tests = append(tests, badMagic)
	reserved := append([]byte(nil), storage[:size]...)
	reserved[28] = 1
	tests = append(tests, reserved)
	for index, data := range tests {
		if _, ok := Decode(data); ok {
			t.Errorf("accepted malformed wire message %d", index)
		}
	}
}
