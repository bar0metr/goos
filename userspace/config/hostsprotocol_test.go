package config

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"strings"
	"testing"
)

func TestHostsRequestRoundTrip(t *testing.T) {
	long := strings.Repeat("a", 61) + "." + strings.Repeat("b", 61) + "." + strings.Repeat("c", 61) + "." + strings.Repeat("d", 61) + "."
	if len(long) != MaxHostName {
		t.Fatal(len(long))
	}
	for _, request := range []HostsRequest{
		{Operation: HostsList, Cursor: 7},
		{Operation: HostsGet, Name: []byte("nas")},
		{Operation: HostsSet, Name: []byte(long), Address: [4]byte{10, 0, 0, 2}},
		{Operation: HostsRemove, Name: []byte("Build.Example.")},
	} {
		message, ok := EncodeHostsRequest(request, 3)
		if !ok {
			t.Fatalf("encode %+v", request)
		}
		if message.PayloadSize > ipc.MaxPayload {
			t.Fatal("payload over the limit")
		}
		decoded, ok := DecodeHostsRequest(&message)
		if !ok || decoded.Operation != request.Operation || string(decoded.Name) != string(request.Name) ||
			decoded.Address != request.Address || decoded.Cursor != request.Cursor {
			t.Fatalf("decoded %+v from %+v", decoded, request)
		}
	}
	// The longest name fills the payload exactly; one more byte is refused.
	message, _ := EncodeHostsRequest(HostsRequest{Operation: HostsGet, Name: []byte(long)}, 1)
	if message.PayloadSize != ipc.MaxPayload {
		t.Fatalf("payload %d", message.PayloadSize)
	}
	if _, ok := EncodeHostsRequest(HostsRequest{Operation: HostsGet, Name: []byte(long + "e")}, 1); ok {
		t.Fatal("a name over MaxHostName encoded")
	}
}

func TestHostsRequestRejections(t *testing.T) {
	for _, request := range []HostsRequest{
		{},
		{Operation: HostsRemove + 1, Name: []byte("a")},
		{Operation: HostsList, Name: []byte("a")},
		{Operation: HostsList, Address: [4]byte{1}},
		{Operation: HostsGet},
		{Operation: HostsGet, Name: []byte("a"), Address: [4]byte{1}},
		{Operation: HostsRemove, Name: []byte("a"), Cursor: 1},
	} {
		if _, ok := EncodeHostsRequest(request, 1); ok {
			t.Fatalf("encoded %+v", request)
		}
	}
	if _, ok := EncodeHostsRequest(HostsRequest{Operation: HostsList}, 0); ok {
		t.Fatal("zero correlation")
	}
	good, _ := EncodeHostsRequest(HostsRequest{Operation: HostsGet, Name: []byte("nas")}, 1)
	for _, damage := range []func(*ipc.Message){
		func(m *ipc.Message) { m.ProtocolID = ProtocolID },
		func(m *ipc.Message) { m.Version++ },
		func(m *ipc.Message) { m.Kind = ipc.KindReply },
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.Payload[3] = 1 },
		func(m *ipc.Message) { m.Payload[2] = 1 },
		func(m *ipc.Message) { m.Payload[4] = 1 },
		func(m *ipc.Message) { m.Payload[1]++ },
		func(m *ipc.Message) { m.PayloadSize++ },
		func(m *ipc.Message) { m.PayloadSize = 7 },
		func(m *ipc.Message) { m.Payload[0] = 0 },
		func(m *ipc.Message) { m.Handles[0], m.HandleCount = handle.ID(5), 1 },
	} {
		message := good
		damage(&message)
		if _, ok := DecodeHostsRequest(&message); ok {
			t.Fatalf("damaged request accepted: % x", message.Payload[:message.PayloadSize])
		}
	}
	// A size field claiming more than MaxHostName is refused even when the
	// payload length agrees with it.
	oversize := good
	oversize.Payload[1], oversize.PayloadSize = MaxHostName+1, hostsHeader+MaxHostName+1
	if _, ok := DecodeHostsRequest(&oversize); ok {
		t.Fatal("oversize name accepted")
	}
}

func TestHostsReplyRoundTrip(t *testing.T) {
	long := strings.Repeat("x", 61) + "." + strings.Repeat("y", 61) + "." + strings.Repeat("z", 61) + "." + strings.Repeat("w", 61) + "."
	for _, reply := range []HostsReply{
		{Operation: HostsList, Status: StatusOK, Name: []byte(long), Address: [4]byte{10, 0, 0, 1}, Next: 65},
		{Operation: HostsList, Status: StatusNotFound, Generation: 9, Next: 3},
		{Operation: HostsGet, Status: StatusOK, Address: [4]byte{127, 0, 0, 1}, Generation: 1 << 40},
		{Operation: HostsSet, Status: StatusResourceExhausted, Generation: 4},
		{Operation: HostsRemove, Status: StatusReadOnly},
	} {
		message, ok := EncodeHostsReply(reply, 8)
		if !ok {
			t.Fatalf("encode %+v", reply)
		}
		if message.PayloadSize > ipc.MaxPayload {
			t.Fatal("payload over the limit")
		}
		decoded, ok := DecodeHostsReply(&message)
		if !ok || decoded.Operation != reply.Operation || decoded.Status != reply.Status || decoded.Generation != reply.Generation ||
			decoded.Address != reply.Address || string(decoded.Name) != string(reply.Name) || decoded.Next != reply.Next {
			t.Fatalf("decoded %+v from %+v", decoded, reply)
		}
	}
	for _, reply := range []HostsReply{
		{Operation: HostsList, Status: StatusOK},
		{Operation: HostsList, Status: StatusOK, Name: []byte("a"), Generation: 1},
		{Operation: HostsGet, Status: StatusOK, Name: []byte("a")},
		{Operation: HostsGet, Status: StatusNotFound, Address: [4]byte{1, 2, 3, 4}},
		{Operation: HostsGet, Status: StatusOK, Next: 1},
		{Operation: HostsGet, Status: StatusReadOnly + 1},
		{Operation: 0},
	} {
		if _, ok := EncodeHostsReply(reply, 1); ok {
			t.Fatalf("encoded %+v", reply)
		}
	}
	good, _ := EncodeHostsReply(HostsReply{Operation: HostsGet, Status: StatusOK, Address: [4]byte{10, 0, 0, 1}, Generation: 2}, 1)
	for _, damage := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Kind = ipc.KindRequest },
		func(m *ipc.Message) { m.Version++ },
		func(m *ipc.Message) { m.PayloadSize-- },
		func(m *ipc.Message) { m.Payload[1] = byte(StatusReadOnly + 1) },
		func(m *ipc.Message) { m.Payload[3] = 1 },
		func(m *ipc.Message) { m.Payload[2] = 1 },
		func(m *ipc.Message) { m.Payload[0] = byte(HostsRemove + 1) },
		func(m *ipc.Message) { m.HandleCount = 1 },
	} {
		message := good
		damage(&message)
		if _, ok := DecodeHostsReply(&message); ok {
			t.Fatalf("damaged reply accepted: % x", message.Payload[:message.PayloadSize])
		}
	}
	// GCF1 and GHST share an endpoint, so neither decoder takes the other's
	// messages.
	if _, ok := DecodeReply(good); ok {
		t.Fatal("GCF1 decoded a hosts reply")
	}
	configRequest, _ := EncodeRequest(Request{Operation: OperationGet, Key: []byte("user.name")}, 1)
	if _, ok := DecodeHostsRequest(&configRequest); ok {
		t.Fatal("GHST decoded a configuration request")
	}
}

func serveHosts(t *testing.T, hosts *Hosts, rights Rights, request HostsRequest) HostsReply {
	t.Helper()
	message, ok := EncodeHostsRequest(request, 1)
	if !ok {
		t.Fatalf("encode %+v", request)
	}
	decoded, ok := DecodeHostsRequest(&message)
	if !ok {
		t.Fatalf("decode %+v", request)
	}
	reply := hosts.Serve(rights, decoded)
	encoded, ok := EncodeHostsReply(reply, 1)
	if !ok {
		t.Fatalf("reply %+v does not encode", reply)
	}
	reply, ok = DecodeHostsReply(&encoded)
	if !ok {
		t.Fatal("reply does not decode")
	}
	return reply
}

func TestHostsServeRights(t *testing.T) {
	var hosts Hosts
	all := Read | Write | HostsRead | HostsWrite
	set := HostsRequest{Operation: HostsSet, Name: []byte("NAS."), Address: [4]byte{10, 0, 0, 2}}
	for _, rights := range []Rights{Read | Write, HostsRead | Read} {
		if reply := serveHosts(t, &hosts, rights, set); reply.Status != StatusPermissionDenied {
			t.Fatalf("set with %b: %v", rights, reply.Status)
		}
	}
	if reply := serveHosts(t, &hosts, Read|Write, HostsRequest{Operation: HostsGet, Name: []byte("localhost")}); reply.Status != StatusPermissionDenied {
		t.Fatal("get without HostsRead")
	}
	if reply := serveHosts(t, &hosts, Read|Write, HostsRequest{Operation: HostsList}); reply.Status != StatusPermissionDenied {
		t.Fatal("list without HostsRead")
	}
	if reply := serveHosts(t, &hosts, HostsWrite, set); reply.Status != StatusOK || reply.Generation != 1 {
		t.Fatalf("set: %+v", reply)
	}
	if reply := serveHosts(t, &hosts, HostsWrite, HostsRequest{Operation: HostsGet, Name: []byte("nas")}); reply.Status != StatusPermissionDenied {
		t.Fatal("get with only HostsWrite")
	}
	reply := serveHosts(t, &hosts, HostsRead, HostsRequest{Operation: HostsGet, Name: []byte("Nas")})
	if reply.Status != StatusOK || reply.Address != [4]byte{10, 0, 0, 2} || reply.Generation != 1 {
		t.Fatalf("get: %+v", reply)
	}
	for _, request := range []HostsRequest{
		{Operation: HostsSet, Name: []byte("localhost"), Address: [4]byte{10, 0, 0, 1}},
		{Operation: HostsRemove, Name: []byte("x.localhost.")},
	} {
		if reply := serveHosts(t, &hosts, all, request); reply.Status != StatusReadOnly {
			t.Fatalf("%+v: %v", request, reply.Status)
		}
	}
	for _, request := range []HostsRequest{
		{Operation: HostsSet, Name: []byte("bad_name"), Address: [4]byte{10, 0, 0, 1}},
		{Operation: HostsSet, Name: []byte("ok"), Address: [4]byte{255, 255, 255, 255}},
		{Operation: HostsGet, Name: []byte("-x")},
	} {
		if reply := serveHosts(t, &hosts, all, request); reply.Status != StatusInvalidArgument {
			t.Fatalf("%+v: %v", request, reply.Status)
		}
	}
	var names []string
	cursor := uint8(0)
	for {
		reply := serveHosts(t, &hosts, HostsRead, HostsRequest{Operation: HostsList, Cursor: cursor})
		if reply.Status == StatusNotFound {
			if reply.Generation != 1 {
				t.Fatalf("end of listing generation %d", reply.Generation)
			}
			break
		}
		if reply.Status != StatusOK || reply.Next <= cursor {
			t.Fatalf("list: %+v", reply)
		}
		names = append(names, string(reply.Name))
		cursor = reply.Next
	}
	if strings.Join(names, " ") != "localhost nas" {
		t.Fatalf("listing %v", names)
	}
	if reply := serveHosts(t, &hosts, HostsWrite, HostsRequest{Operation: HostsRemove, Name: []byte("NAS")}); reply.Status != StatusOK || reply.Generation != 2 {
		t.Fatalf("remove: %+v", reply)
	}
}

func TestSessionRightsIncludeHosts(t *testing.T) {
	scope, _ := MakeScope("")
	for _, rights := range []Rights{Read | HostsRead, Read | Write | HostsRead | HostsWrite, HostsRead} {
		message, ok := AddSessionRequest(handle.ID(4), scope, rights, 1)
		if !ok {
			t.Fatalf("encode %b", rights)
		}
		if _, _, decoded, ok := DecodeAddSession(message); !ok || decoded != rights {
			t.Fatalf("decode %b = %b %v", rights, decoded, ok)
		}
	}
	if _, ok := AddSessionRequest(handle.ID(4), scope, HostsWrite<<1, 1); ok {
		t.Fatal("unknown right encoded")
	}
	message, _ := AddSessionRequest(handle.ID(4), scope, Read, 1)
	message.Payload[1] = byte(Read | HostsWrite<<1)
	if _, _, _, ok := DecodeAddSession(message); ok {
		t.Fatal("unknown right decoded")
	}
	if HostsRead != 4 || HostsWrite != 8 {
		t.Fatal("rights values moved")
	}
}
