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
		{Operation: OperationRead, Node: 9, Offset: 1 << 20, Size: PageSize, Slot: CachePages - 1},
		{Operation: OperationWrite, Node: 9, Offset: 4, Size: PageSize - 4, Slot: 3},
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
			decoded.Offset != request.Offset || decoded.Size != request.Size || decoded.Slot != request.Slot || string(decoded.Name) != string(request.Name) {
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
		{Operation: OperationRead, Size: PageSize + 1},
		{Operation: OperationRead, Size: 0},
		{Operation: OperationRead, Offset: 1, Size: PageSize},      // crosses a page
		{Operation: OperationWrite, Offset: PageSize - 1, Size: 2}, // crosses a page
		{Operation: OperationWrite, Size: 1, Slot: CachePages},     // no such slot
		{Operation: OperationStat, Slot: 1},                        // a slot where none is used
	} {
		if EncodeRequest(request, 1, &message) {
			t.Fatalf("encoded %#v", request)
		}
	}
	if EncodeRequest(Request{Operation: OperationMount}, 0, &message) {
		t.Fatal("encoded an uncorrelated request")
	}
	if EncodeReply(Reply{Operation: OperationStat, Status: ramfs.LastStatus + 1}, 1, &message) {
		t.Fatal("encoded an unknown status")
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
		func(m *ipc.Message) { m.Payload[2] = 1 },
		func(m *ipc.Message) { m.Payload[4] = 1 }, // a slot on an open
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
		func(m *ipc.Message) { m.Payload[1] = byte(ramfs.LastStatus + 1) },
		func(m *ipc.Message) { m.Payload[2] = byte(ramfs.KindDirectory + 1) },
		func(m *ipc.Message) { m.Payload[5] = 1 },
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
	var name [ramfs.MaxName]byte
	cache := make([]byte, CacheSize)
	volume := &refusedVolume{}
	if reply := Serve(volume, Request{Operation: OperationMount}, cache, &name); reply.Status != ramfs.StatusIOError {
		t.Fatalf("mount of a refused volume = %d", reply.Status)
	}
	if reply := Serve(volume, Request{Operation: OperationRead, Size: 8}, cache, &name); reply.Status != ramfs.StatusIOError || reply.Value != 0 {
		t.Fatalf("overlong read = %d, %d bytes", reply.Status, reply.Value)
	}
	if reply := Serve(volume, Request{Operation: OperationRead, Size: 8}, nil, &name); reply.Status != ramfs.StatusIOError {
		t.Fatalf("read without a cache = %d", reply.Status)
	}
	if reply := Serve(volume, Request{Operation: OperationReadDir}, cache, &name); reply.Status != ramfs.StatusIOError {
		t.Fatalf("unsendable cursor = %d", reply.Status)
	}
	if reply := Serve(volume, Request{Operation: OperationOpen}, cache, &name); reply.Status != ramfs.StatusInvalidArgument {
		t.Fatalf("open without a name = %d", reply.Status)
	}
	if reply := Serve(volume, Request{Operation: OperationFlush}, cache, &name); reply.Status != ramfs.StatusOK || !volume.flushed {
		t.Fatal("flush did not reach the device")
	}
}

// A transfer moves exactly the bytes at its offset's place in its own slot.
func TestSlotBytesStayInTheirSlot(t *testing.T) {
	cache := make([]byte, CacheSize)
	bytes, ok := SlotBytes(cache, Request{Operation: OperationWrite, Offset: 3*PageSize + 100, Size: 50, Slot: 2})
	if !ok || len(bytes) != 50 || &bytes[0] != &cache[2*PageSize+100] {
		t.Fatal("write bytes not at the offset within slot 2")
	}
	for _, request := range []Request{
		{Operation: OperationRead, Size: PageSize, Slot: CachePages},
		{Operation: OperationRead, Offset: PageSize - 1, Size: 2},
		{Operation: OperationStat},
	} {
		if _, ok := SlotBytes(cache, request); ok {
			t.Fatalf("accepted %#v", request)
		}
	}
	if _, ok := SlotBytes(cache[:PageSize], Request{Operation: OperationRead, Size: 1}); ok {
		t.Fatal("accepted a cache of the wrong size")
	}
}
