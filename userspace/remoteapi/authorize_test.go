package remoteapi

import (
	"goos/kernel/ipc"
	"testing"
)

func authorizeRequest(t *testing.T, username string, key byte) Request {
	t.Helper()
	request := Request{Operation: OperationAuthorizeKey}
	request.UserSize = uint8(copy(request.Username[:], username))
	for i := range request.Key {
		request.Key[i] = key
	}
	return request
}

func TestAuthorizeKeyRoundTrip(t *testing.T) {
	request := authorizeRequest(t, "alice", 7)
	var message ipc.Message
	if !EncodeRequest(&request, 7, &message) {
		t.Fatal("authorize encode")
	}
	var decoded Request
	if !DecodeRequest(&message, &decoded) || decoded != request {
		t.Fatalf("authorize round trip: got %+v want %+v", decoded, request)
	}
	if decoded.UserSize != 5 || string(decoded.Username[:decoded.UserSize]) != "alice" {
		t.Fatalf("authorize username lost: %q", decoded.Username[:decoded.UserSize])
	}
}

func TestAuthorizeKeyReplyRoundTrip(t *testing.T) {
	for _, result := range []Result{ResultOK, ResultPermissionDenied} {
		reply := Reply{Operation: OperationAuthorizeKey, Result: result}
		var message ipc.Message
		if !EncodeReply(&reply, nil, 3, &message) {
			t.Fatal("authorize reply encode", result)
		}
		var decoded Reply
		if !DecodeReply(&message, 3, OperationAuthorizeKey, &decoded) || decoded.Result != result || decoded.Session != 0 {
			t.Fatalf("authorize reply round trip: got %+v want %v", decoded, result)
		}
	}
	// An authorize reply must not mint a session.
	var message ipc.Message
	if EncodeReply(&Reply{Operation: OperationAuthorizeKey, Result: ResultOK, Session: 1}, nil, 3, &message) {
		t.Fatal("authorize reply carried a session")
	}
}

func TestAuthorizeKeyRejectsMalformed(t *testing.T) {
	request := authorizeRequest(t, "alice", 7)
	var message ipc.Message
	if !EncodeRequest(&request, 7, &message) {
		t.Fatal("encode")
	}
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Version-- },        // previous protocol version is rejected
		func(m *ipc.Message) { m.Payload[1] = 0 },   // empty username
		func(m *ipc.Message) { m.Payload[1] = 33 },  // username past MaxUsername
		func(m *ipc.Message) { m.PayloadSize-- },    // truncated key
		func(m *ipc.Message) { m.Payload[0] = 255 }, // unknown operation
		func(m *ipc.Message) { m.Payload[8] = 1 },   // session selected
		func(m *ipc.Message) { m.Payload[2] = 1 },   // non-terminal mode
		func(m *ipc.Message) { m.Payload[20] = 1 },  // reserved byte
	} {
		bad := message
		mutate(&bad)
		var decoded Request
		if DecodeRequest(&bad, &decoded) {
			t.Fatal("malformed authorize request accepted")
		}
	}
	// A username with an invalid byte is refused.
	bad := authorizeRequest(t, "alice/x", 7)
	if EncodeRequest(&bad, 7, &message) {
		t.Fatal("invalid username accepted")
	}
}

func TestAuthenticateCarriesUsernameAndKey(t *testing.T) {
	request := Request{Operation: OperationAuthenticate}
	request.UserSize = uint8(copy(request.Username[:], "bob"))
	copy(request.Key[:], []byte("0123456789abcdef0123456789abcdef"))
	var message ipc.Message
	if !EncodeRequest(&request, 5, &message) {
		t.Fatal("authenticate encode")
	}
	var decoded Request
	if !DecodeRequest(&message, &decoded) || decoded != request {
		t.Fatalf("authenticate round trip: got %+v want %+v", decoded, request)
	}
	// A key for one name is a different, well-formed request; the admission
	// layer, not the codec, decides the isolation boundary. The codec must
	// preserve both fields exactly.
	request.Username[0] = 'B'
	if !EncodeRequest(&request, 5, &message) {
		t.Fatal("authenticate encode variant")
	}
	if !DecodeRequest(&message, &decoded) || decoded != request {
		t.Fatal("authenticate variant round trip")
	}
}
