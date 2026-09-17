package block

import (
	"goos/kernel/ipc"
	"testing"
)

func TestProtocolRoundTrip(t *testing.T) {
	requestMessage, ok := EncodeRequest(Request{Operation: OperationWrite, Offset: 31, Length: 4, Data: []byte("data")}, 7)
	if !ok {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequest(requestMessage)
	if !ok || request.Operation != OperationWrite || request.Offset != 31 || string(request.Data) != "data" {
		t.Fatalf("request = %#v", request)
	}
	replyMessage, ok := EncodeReply(Reply{Operation: OperationRead, Status: StatusOK, Capacity: 4096, BlockSize: 512, Data: []byte("data")}, 7)
	if !ok {
		t.Fatal("reply encode failed")
	}
	reply, ok := DecodeReply(replyMessage)
	if !ok || reply.Capacity != 4096 || reply.BlockSize != 512 || string(reply.Data) != "data" {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestProtocolRejectsMalformedMessages(t *testing.T) {
	if _, ok := EncodeRequest(Request{Operation: OperationRead, Length: MaxData + 1}, 1); ok {
		t.Fatal("oversized read accepted")
	}
	if _, ok := EncodeRequest(Request{Operation: OperationWrite, Length: 2, Data: []byte("x")}, 1); ok {
		t.Fatal("mismatched write accepted")
	}
	message, _ := EncodeRequest(Request{Operation: OperationInfo}, 1)
	message.Version++
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("unknown version accepted")
	}
	message = ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: 1, PayloadSize: headerSize}
	message.Payload[1] = 1
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("non-zero reserved field accepted")
	}
}
