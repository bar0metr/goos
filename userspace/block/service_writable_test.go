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

// A version 2 reply is refused: version 3 gave byte 2 of the Info reply the
// writable flag, which version 2 required to be zero.
func TestReplyRefusesVersion2(t *testing.T) {
	message, ok := EncodeReply(Reply{Operation: OperationInfo, Capacity: 4096, BlockSize: 512, Writable: true}, 7)
	if !ok || message.Version != 3 {
		t.Fatalf("encoded version %d", message.Version)
	}
	message.Version = 2
	if _, ok := DecodeReply(message); ok {
		t.Fatal("version 2 reply accepted")
	}
	request, _ := EncodeRequest(Request{Operation: OperationInfo}, 8)
	request.Version = 2
	if _, ok := DecodeRequest(request); ok {
		t.Fatal("version 2 request accepted")
	}
}
