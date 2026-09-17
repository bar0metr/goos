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

func TestProtocolPointerRoundTrip(t *testing.T) {
	var requestMessage ipc.Message
	if !EncodeRequestInto(Request{Operation: OperationRead, Offset: 7, Length: 4}, 9, &requestMessage) {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequestFrom(&requestMessage)
	if !ok || request.Offset != 7 || request.Length != 4 {
		t.Fatalf("request = %#v", request)
	}
	var replyMessage ipc.Message
	if !EncodeReplyInto(Reply{Operation: OperationRead, Data: []byte("data")}, 9, &replyMessage) {
		t.Fatal("reply encode failed")
	}
	reply, ok := DecodeReplyFrom(&replyMessage)
	if !ok || string(reply.Data) != "data" {
		t.Fatalf("reply = %#v", reply)
	}
	if EncodeRequestInto(Request{Operation: OperationInfo}, 1, nil) || EncodeReplyInto(Reply{Operation: OperationInfo}, 1, nil) {
		t.Fatal("nil message accepted")
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
