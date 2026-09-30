package linux

import (
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/vfs"
)

func vfsReplyMessage(t *testing.T, reply vfs.Reply, correlation uint64) ipc.Message {
	t.Helper()
	message, ok := vfs.EncodeReply(reply, correlation)
	if !ok {
		t.Fatal("reply did not encode")
	}
	return message
}

func TestVFSSessionCorrelationsRiseAndStopAfterFailure(t *testing.T) {
	session := VFSSession{Handle: 9}
	first, ok := session.Next()
	second, again := session.Next()
	if !ok || !again || first != 1 || second != 2 || session.Failed() {
		t.Fatalf("correlations = %d,%d ok=%v,%v", first, second, ok, again)
	}
	session.Fail()
	if _, ok := session.Next(); ok || !session.Failed() {
		t.Fatal("failed session issued a correlation")
	}
	if _, ok := (&VFSSession{}).Next(); ok {
		t.Fatal("session without a handle issued a correlation")
	}
	var absent *VFSSession
	if _, ok := absent.Next(); ok || !absent.Failed() {
		t.Fatal("absent session is usable")
	}
}

func TestClassifyVFSReply(t *testing.T) {
	open := vfs.Reply{Operation: vfs.OperationOpen, Status: vfs.StatusOK, Handle: 0x10001}
	message := vfsReplyMessage(t, open, 7)
	reply, verdict := ClassifyVFSReply(&message, 7, vfs.OperationOpen)
	if verdict != VFSReplyAccepted || reply.Handle != 0x10001 {
		t.Fatalf("matching reply: verdict=%d reply=%#v", verdict, reply)
	}
	// A reply to a request the caller gave up on is dropped, not failed.
	if _, verdict := ClassifyVFSReply(&message, 8, vfs.OperationOpen); verdict != VFSReplyStale {
		t.Fatalf("older correlation: verdict=%d", verdict)
	}
	// No request with a later correlation was ever sent.
	if _, verdict := ClassifyVFSReply(&message, 6, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("future correlation: verdict=%d", verdict)
	}
	if _, verdict := ClassifyVFSReply(&message, 7, vfs.OperationClose); verdict != VFSReplyMalformed {
		t.Fatalf("other operation: verdict=%d", verdict)
	}
	if _, verdict := ClassifyVFSReply(&message, 0, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("zero correlation: verdict=%d", verdict)
	}
	attached := message
	attached.HandleCount = 1
	if _, verdict := ClassifyVFSReply(&attached, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("attached handle: verdict=%d", verdict)
	}
	request, ok := vfs.EncodeRequest(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/x")}, 7)
	if !ok {
		t.Fatal("request did not encode")
	}
	if _, verdict := ClassifyVFSReply(&request, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("request as reply: verdict=%d", verdict)
	}
	foreign := message
	foreign.ProtocolID++
	if _, verdict := ClassifyVFSReply(&foreign, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("foreign protocol: verdict=%d", verdict)
	}
	if _, verdict := ClassifyVFSReply(nil, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("nil message: verdict=%d", verdict)
	}
}

func TestVFSCountersSaturate(t *testing.T) {
	vfsDiagnostics = VFSDiagnostics{Requests: ^uint64(0) - 1}
	NoteVFSRequest()
	NoteVFSRequest()
	NoteVFSFailure()
	NoteVFSStale()
	if got := VFSStatistics(); got.Requests != ^uint64(0) || got.Failures != 1 || got.Stale != 1 {
		t.Fatalf("statistics = %#v", got)
	}
	vfsDiagnostics = VFSDiagnostics{}
}
