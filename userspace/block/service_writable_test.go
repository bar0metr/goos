package block

import "testing"

func TestInfoReportsSliceWritability(t *testing.T) {
	for _, writable := range []bool{false, true} {
		reply := Reply{Operation: OperationInfo, Capacity: 4096, BlockSize: 512, Writable: writable}
		message, ok := EncodeReply(reply, 7)
		if !ok {
			t.Fatal("encode")
		}
		decoded, ok := DecodeReply(message)
		if !ok || decoded.Writable != writable {
			t.Fatalf("writable %v decoded as %+v", writable, decoded)
		}
	}
	if _, ok := EncodeReply(Reply{Operation: OperationRead, Writable: true}, 7); ok {
		t.Fatal("writable flag on a read reply")
	}
}
