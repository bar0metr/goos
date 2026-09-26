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

// TestEncodeRequestClearsReusedMessage covers the freestanding services, which
// reuse message storage and once shipped stale reserved bytes that the peer
// correctly rejected and silently dropped.
func TestEncodeRequestClearsReusedMessage(t *testing.T) {
	var message ipc.Message
	for index := range message.Payload {
		message.Payload[index] = 0xab
	}
	message.HandleCount = 2
	message.Handles[0] = 7
	if !EncodeRequestInto(Request{Operation: OperationRead, Offset: 512, Length: 32}, 9, &message) {
		t.Fatal("encode failed")
	}
	request, ok := DecodeRequestFrom(&message)
	if !ok || request.Operation != OperationRead || request.Offset != 512 || request.Length != 32 || len(request.Data) != 0 {
		t.Fatalf("request = %+v ok=%v", request, ok)
	}
	if message.HandleCount != 0 || message.Handles[0] != 0 {
		t.Fatal("stale handle attachments survived encoding")
	}
	for index := headerSize; index < len(message.Payload); index++ {
		if message.Payload[index] != 0 {
			t.Fatalf("stale payload byte at %d", index)
		}
	}
}

func TestControlReplacesTheDriverEndpoint(t *testing.T) {
	message, ok := SetDriverRequest(9, 4)
	if !ok {
		t.Fatal("encode failed")
	}
	driver, valid := DecodeSetDriver(message)
	if !valid || driver != 9 {
		t.Fatalf("decoded %d %v", driver, valid)
	}
	if _, ok := SetDriverRequest(0, 4); ok {
		t.Fatal("a request with no endpoint was encoded")
	}
	if _, ok := SetDriverRequest(9, 0); ok {
		t.Fatal("a request with no correlation was encoded")
	}
	// The attached handle is the whole point of the message.
	corrupt := message
	corrupt.HandleCount = 0
	if _, ok := DecodeSetDriver(corrupt); ok {
		t.Fatal("decoded a request with no endpoint")
	}
	corrupt = message
	corrupt.Payload[0] = 2
	if _, ok := DecodeSetDriver(corrupt); ok {
		t.Fatal("decoded an unknown control operation")
	}
	corrupt = message
	corrupt.ProtocolID = ProtocolID
	if _, ok := DecodeSetDriver(corrupt); ok {
		t.Fatal("decoded a data request as control")
	}
	reply, ok := SetDriverReply(StatusIOError, 4)
	if status, valid := DecodeSetDriverReply(reply); !ok || !valid || status != StatusIOError {
		t.Fatalf("reply round trip: %d %v", status, valid)
	}
}

func TestSetFilesystemControl(t *testing.T) {
	message, ok := SetFilesystemRequest(12, 5)
	if !ok {
		t.Fatal("encode failed")
	}
	server, valid := DecodeSetFilesystem(message)
	if !valid || server != 12 {
		t.Fatalf("decoded %d %v", server, valid)
	}
	if _, ok := DecodeSetDriver(message); ok {
		t.Fatal("a filesystem endpoint was taken for a driver")
	}
	if _, ok := SetFilesystemRequest(0, 5); ok {
		t.Fatal("a request with no endpoint was encoded")
	}
	corrupt := message
	corrupt.HandleCount = 0
	if _, ok := DecodeSetFilesystem(corrupt); ok {
		t.Fatal("decoded a request with no endpoint")
	}
}

func TestBulkRequestsCarryExactlyOneObject(t *testing.T) {
	request := Request{Operation: OperationReadBulk, Offset: 4096, Length: 65536, Buffer: 8192, Object: 0x100000007}
	message, ok := EncodeRequest(request, 9)
	if !ok || message.HandleCount != 1 || message.Handles[0] != request.Object {
		t.Fatalf("encode: %v %+v", ok, message)
	}
	decoded, ok := DecodeRequest(message)
	if !ok || decoded.Operation != OperationReadBulk || decoded.Offset != 4096 || decoded.Length != 65536 || decoded.Buffer != 8192 || decoded.Object != request.Object {
		t.Fatalf("decode: %v %+v", ok, decoded)
	}
	missing := message
	missing.HandleCount = 0
	if _, ok := DecodeRequest(missing); ok {
		t.Fatal("a bulk request without its object was accepted")
	}
	inline, _ := EncodeRequest(Request{Operation: OperationRead, Length: 8}, 10)
	inline.HandleCount, inline.Handles[0] = 1, 5
	if _, ok := DecodeRequest(inline); ok {
		t.Fatal("an inline request carrying a handle was accepted")
	}
	inline, _ = EncodeRequest(Request{Operation: OperationRead, Length: 8}, 10)
	inline.Payload[4] = 1
	if _, ok := DecodeRequest(inline); ok {
		t.Fatal("an inline request with a buffer offset was accepted")
	}
	for _, bad := range []Request{
		{Operation: OperationWriteBulk, Length: 0, Object: 1},
		{Operation: OperationWriteBulk, Length: MaxBulk + 1, Object: 1},
		{Operation: OperationWriteBulk, Length: 1},
		{Operation: OperationWriteBulk, Length: 1, Object: 1, Data: []byte{1}},
	} {
		if _, ok := EncodeRequest(bad, 11); ok {
			t.Fatalf("%+v encoded", bad)
		}
	}
	reply, ok := EncodeReply(Reply{Operation: OperationWriteBulk}, 12)
	if decodedReply, valid := DecodeReply(reply); !ok || !valid || decodedReply.Operation != OperationWriteBulk {
		t.Fatal("bulk reply")
	}
}
