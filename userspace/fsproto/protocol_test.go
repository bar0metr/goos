package fsproto

import (
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	requests := []Request{
		{Operation: OperationMount},
		{Operation: OperationOpen, Parent: 7, Name: []byte("notes.txt")},
		{Operation: OperationRead, Node: 9, Offset: 1 << 20, Size: MaxReplyData},
		{Operation: OperationWrite, Node: 9, Offset: 4, Data: make([]byte, MaxRequestData)},
		{Operation: OperationRename, Node: 9, Parent: 3, Name: []byte("renamed")},
		{Operation: OperationReadDir, Parent: 1, Size: 12},
		{Operation: OperationFlush},
	}
	for _, request := range requests {
		var message ipc.Message
		if !EncodeRequest(request, 42, &message) {
			t.Fatalf("encode %#v", request)
		}
		decoded, ok := DecodeRequest(&message)
		if !ok || decoded.Operation != request.Operation || decoded.Node != request.Node || decoded.Parent != request.Parent ||
			decoded.Offset != request.Offset || decoded.Size != request.Size || string(decoded.Name) != string(request.Name) || len(decoded.Data) != len(request.Data) {
			t.Fatalf("round trip %#v = %#v, %v", request, decoded, ok)
		}
	}
}

func TestReplyRoundTrip(t *testing.T) {
	reply := Reply{Operation: OperationReadDir, Status: ramfs.StatusOK, Kind: ramfs.KindDirectory, Node: 0x1_0000_0002, Value: 4096, Cursor: 3, Name: []byte("home")}
	var message ipc.Message
	if !EncodeReply(reply, 5, &message) {
		t.Fatal("encode")
	}
	decoded, ok := DecodeReply(&message)
	if !ok || decoded.Node != reply.Node || decoded.Kind != reply.Kind || decoded.Value != reply.Value || decoded.Cursor != reply.Cursor || string(decoded.Name) != "home" {
		t.Fatalf("round trip = %#v, %v", decoded, ok)
	}
}

func TestEncodeRefusesWhatCannotBeSent(t *testing.T) {
	var message ipc.Message
	for _, request := range []Request{
		{},
		{Operation: OperationFlush + 1},
		{Operation: OperationOpen, Name: []byte("a/b")},
		{Operation: OperationOpen, Name: []byte("..")},
		{Operation: OperationRead, Size: MaxReplyData + 1},
		{Operation: OperationWrite, Data: make([]byte, MaxRequestData+1)},
	} {
		if EncodeRequest(request, 1, &message) {
			t.Fatalf("encoded %#v", request)
		}
	}
	if EncodeRequest(Request{Operation: OperationMount}, 0, &message) {
		t.Fatal("encoded an uncorrelated request")
	}
	if EncodeReply(Reply{Operation: OperationStat, Status: ramfs.StatusIOError + 1}, 1, &message) {
		t.Fatal("encoded an unknown status")
	}
	if EncodeReply(Reply{Operation: OperationRead, Data: make([]byte, MaxReplyData+1)}, 1, &message) {
		t.Fatal("encoded an oversized reply")
	}
}

// Every field an attacker controls is checked: a service or a VFS that
// receives a damaged message refuses it instead of acting on it.
func TestDecodeRefusesMalformedMessages(t *testing.T) {
	var valid ipc.Message
	if !EncodeRequest(Request{Operation: OperationOpen, Parent: 1, Name: []byte("name")}, 3, &valid) {
		t.Fatal("encode")
	}
	damage := []func(*ipc.Message){
		func(m *ipc.Message) { m.ProtocolID++ },
		func(m *ipc.Message) { m.Version++ },
		func(m *ipc.Message) { m.Kind = ipc.KindReply },
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.HandleCount = 1 },
		func(m *ipc.Message) { m.Payload[0] = 0 },
		func(m *ipc.Message) { m.Payload[0] = byte(OperationFlush + 1) },
		func(m *ipc.Message) { m.Payload[1] = ramfs.MaxName + 1 },
		func(m *ipc.Message) { m.Payload[5] = 1 },
		func(m *ipc.Message) { m.Payload[37] = 1 },
		func(m *ipc.Message) { m.PayloadSize++ },
		func(m *ipc.Message) { m.PayloadSize = requestHeader - 1 },
		func(m *ipc.Message) { m.Payload[requestHeader] = '/' },
	}
	for index, apply := range damage {
		message := valid
		apply(&message)
		if _, ok := DecodeRequest(&message); ok {
			t.Fatalf("damage %d accepted", index)
		}
	}
	var reply ipc.Message
	if !EncodeReply(Reply{Operation: OperationStat, Kind: ramfs.KindFile, Value: 1}, 3, &reply) {
		t.Fatal("encode reply")
	}
	for index, apply := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Payload[1] = byte(ramfs.StatusIOError + 1) },
		func(m *ipc.Message) { m.Payload[2] = byte(ramfs.KindDirectory + 1) },
		func(m *ipc.Message) { m.Payload[7] = 1 },
		func(m *ipc.Message) { m.Kind = ipc.KindRequest },
		func(m *ipc.Message) { m.PayloadSize-- },
	} {
		message := reply
		apply(&message)
		if _, ok := DecodeReply(&message); ok {
			t.Fatalf("reply damage %d accepted", index)
		}
	}
}

// refusedVolume is a volume whose filesystem was not accepted.
type refusedVolume struct{ flushed bool }

func (*refusedVolume) Root() (ramfs.NodeID, ramfs.Status) { return 0, ramfs.StatusIOError }
func (*refusedVolume) OpenName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, ramfs.StatusIOError
}
func (*refusedVolume) CreateName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, ramfs.StatusIOError
}
func (*refusedVolume) MkdirName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, ramfs.StatusIOError
}
func (*refusedVolume) Read(ramfs.NodeID, uint64, []byte) (int, ramfs.Status) {
	return 1 << 20, ramfs.StatusOK // a backend reporting more than it could have read
}
func (*refusedVolume) Write(ramfs.NodeID, uint64, []byte) (int, ramfs.Status) {
	return 0, ramfs.StatusIOError
}
func (*refusedVolume) Truncate(ramfs.NodeID, uint64) ramfs.Status { return ramfs.StatusIOError }
func (*refusedVolume) Stat(ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	return ramfs.Stat{}, ramfs.StatusIOError
}
func (*refusedVolume) ReadDir(ramfs.NodeID, int) (ramfs.Entry, int, ramfs.Status) {
	return ramfs.Entry{}, -1, ramfs.StatusOK // a cursor that cannot be sent
}
func (*refusedVolume) RenameName(ramfs.NodeID, ramfs.NodeID, ramfs.Name) ramfs.Status {
	return ramfs.StatusIOError
}
func (*refusedVolume) UnlinkName(ramfs.NodeID, ramfs.Name) ramfs.Status { return ramfs.StatusIOError }
func (volume *refusedVolume) Flush() bool                               { volume.flushed = true; return true }

func TestServeReportsWhatTheVolumeCannotDo(t *testing.T) {
	var scratch [MaxReplyData]byte
	volume := &refusedVolume{}
	if reply := Serve(volume, Request{Operation: OperationMount}, &scratch); reply.Status != ramfs.StatusIOError {
		t.Fatalf("mount of a refused volume = %d", reply.Status)
	}
	if reply := Serve(volume, Request{Operation: OperationRead, Size: 8}, &scratch); reply.Status != ramfs.StatusIOError || len(reply.Data) != 0 {
		t.Fatalf("overlong read = %d, %d bytes", reply.Status, len(reply.Data))
	}
	if reply := Serve(volume, Request{Operation: OperationReadDir}, &scratch); reply.Status != ramfs.StatusIOError {
		t.Fatalf("unsendable cursor = %d", reply.Status)
	}
	if reply := Serve(volume, Request{Operation: OperationOpen}, &scratch); reply.Status != ramfs.StatusInvalidArgument {
		t.Fatalf("open without a name = %d", reply.Status)
	}
	if reply := Serve(volume, Request{Operation: OperationFlush}, &scratch); reply.Status != ramfs.StatusOK || !volume.flushed {
		t.Fatal("flush did not reach the device")
	}
}
