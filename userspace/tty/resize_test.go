package tty

import (
	"goos/kernel/ipc"
	"testing"
)

func remoteSize(sequence uint64, columns, rows int) Geometry {
	return Geometry{Sequence: sequence, Columns: columns, Rows: rows, Authoritative: true, Term: []byte("xterm")}
}

func TestSnapshotStartupRaceStaleAndDuplicate(t *testing.T) {
	var snapshot Snapshot
	if !snapshot.Apply(remoteSize(12, 120, 40)) {
		t.Fatal("early notification refused")
	}
	for _, sequence := range []uint64{10, 11, 12, 1} {
		if snapshot.Apply(remoteSize(sequence, 80, 24)) {
			t.Fatalf("stale snapshot %d applied", sequence)
		}
	}
	geometry, _ := snapshot.Current()
	if geometry.Sequence != 12 || geometry.Columns != 120 || geometry.Rows != 40 {
		t.Fatalf("geometry %+v", geometry)
	}
	if !snapshot.Apply(remoteSize(13, 1, 1000)) {
		t.Fatal("newest snapshot refused")
	}
}

func TestGeometryNoticeHasNoInputCorrelationOrAuthority(t *testing.T) {
	message, valid := GeometryNotice(remoteSize(1<<63, 120, 40))
	geometry, decoded := DecodeGeometryNotice(&message)
	if !valid || !decoded || geometry.Sequence != 1<<63 || message.Correlation != 0 || message.HandleCount != 0 {
		t.Fatal("invalid notification")
	}
	if _, _, valid := DecodeReply(&message); valid {
		t.Fatal("notification mistaken for input reply")
	}
	message.HandleCount = 1
	if _, valid := DecodeGeometryNotice(&message); valid {
		t.Fatal("attached authority accepted")
	}
}

func TestEngineCoalescesBackpressuredResizeWithoutDelayingInput(t *testing.T) {
	blocked := false
	var replies []ipc.Message
	engine := NewEngine(func([]byte) bool { return true }, func(message ipc.Message) bool {
		if message.Kind == ipc.KindNotification && blocked {
			return false
		}
		replies = append(replies, message)
		return true
	})
	engine.SetRemoteGeometry(80, 24, []byte("xterm"))
	request, _ := Request(OperationGeometry, nil, 1)
	engine.Request(&request)
	request, _ = Request(OperationRawMode, nil, 2)
	engine.Request(&request)
	request, _ = Request(OperationReadLine, nil, 3)
	engine.Request(&request)
	blocked = true
	for _, size := range [][2]int{{120, 40}, {90, 30}, {130, 45}} {
		if !engine.SetRemoteGeometry(size[0], size[1], []byte("xterm")) {
			t.Fatal("resize blocked input")
		}
		engine.FlushGeometry()
	}
	engine.Feed('x')
	engine.FlushInput()
	if engine.Failed() || !engine.GeometryPending() {
		t.Fatal("backpressure failed engine or lost resize")
	}
	result, text, valid := DecodeReply(&replies[len(replies)-1])
	if !valid || result != ResultRaw || string(text) != "x" {
		t.Fatal("input delayed")
	}
	blocked = false
	engine.FlushGeometry()
	geometry, valid := DecodeGeometryNotice(&replies[len(replies)-1])
	if !valid || geometry.Sequence != 4 || geometry.Columns != 130 || geometry.Rows != 45 || engine.GeometryPending() {
		t.Fatalf("latest geometry %+v", geometry)
	}
}

func TestSnapshotCopiesTermAndResetStartsNewSession(t *testing.T) {
	var snapshot Snapshot
	geometry := remoteSize(99, 80, 24)
	snapshot.Apply(geometry)
	geometry.Term[0] = 'X'
	copied, _ := snapshot.Current()
	if string(copied.Term) != "xterm" {
		t.Fatal("TERM borrowed across receive")
	}
	snapshot.Reset()
	if !snapshot.Apply(remoteSize(1, 80, 24)) {
		t.Fatal("new session retained old sequence")
	}
}

func TestRawResizeRestoresCookedInputWithNewestGeometry(t *testing.T) {
	h := newEngineHarness()
	h.engine.SetRemoteGeometry(80, 24, []byte("xterm"))
	h.request(t, OperationGeometry, "", 1)
	h.request(t, OperationRawMode, "", 2)
	h.engine.SetRemoteGeometry(120, 40, []byte("xterm"))
	h.engine.SetRemoteGeometry(130, 45, []byte("xterm"))
	h.request(t, OperationLineMode, "", 3)
	columns, rows := h.engine.screen.Geometry()
	if h.engine.Raw() || columns != 130 || rows != 45 {
		t.Fatal("cooked mode restored old geometry")
	}
	h.request(t, OperationReadLine, "> ", 4)
	h.feed("ok\n")
	h.reply(t, len(h.replies)-1, ResultLine, "ok", 4)
}

func TestGeometryNoticesReserveSharedQueueForRawInput(t *testing.T) {
	channel := ipc.NewChannel()
	engine := NewEngine(func([]byte) bool { return true }, func(message ipc.Message) bool { return channel.Send(0, message, nil, nil, nil) == ipc.StatusOK })
	engine.SetRemoteGeometry(80, 24, []byte("xterm"))
	for index, operation := range []Operation{OperationGeometry, OperationRawMode, OperationReadLine} {
		request, _ := Request(operation, nil, uint64(index+1))
		if channel.Send(1, request, nil, nil, nil) != ipc.StatusOK {
			t.Fatal("request send")
		}
		received, status := channel.Receive(0)
		if status != ipc.StatusOK {
			t.Fatal("request receive")
		}
		engine.Request(&received)
	}
	for count := 0; count < 64; count++ {
		engine.SetRemoteGeometry(120+count, 40, []byte("xterm"))
		engine.FlushGeometry()
	}
	engine.Feed('x')
	engine.FlushInput()
	if engine.Failed() {
		t.Fatal("resize filled shared input reply queue")
	}
	var notice Geometry
	raw := false
	for count := 0; count < 4; count++ {
		message, status := channel.Receive(1)
		if status != ipc.StatusOK {
			t.Fatal("missing queued response")
		}
		if geometry, valid := DecodeGeometryNotice(&message); valid {
			notice = geometry
		}
		if result, text, valid := DecodeReply(&message); valid && result == ResultRaw && string(text) == "x" {
			raw = true
		}
	}
	if !raw || notice.Sequence != 2 {
		t.Fatal("one queued notice did not preserve raw input")
	}
	ack := GeometryAcknowledgement(notice.Sequence)
	engine.Request(&ack)
	engine.FlushGeometry()
	message, status := channel.Receive(1)
	geometry, valid := DecodeGeometryNotice(&message)
	if status != ipc.StatusOK || !valid || geometry.Sequence != 65 || geometry.Columns != 183 {
		t.Fatalf("coalesced snapshot %+v", geometry)
	}
}

func TestResizeFrameRoundTripAndPadding(t *testing.T) {
	var frame [ResizeFrameSize]byte
	if ResizeFrameSize > 256 || !EncodeResizeFrame(remoteSize(7, 120, 40), frame[:]) {
		t.Fatal("frame exceeds atomic write")
	}
	geometry, valid := DecodeResizeFrame(frame[:])
	if !valid || geometry.Sequence != 7 || geometry.Columns != 120 {
		t.Fatal("frame roundtrip")
	}
	frame[len(frame)-1] = 1
	if _, valid := DecodeResizeFrame(frame[:]); valid {
		t.Fatal("nonzero padding accepted")
	}
}
