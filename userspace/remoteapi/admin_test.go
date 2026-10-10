package remoteapi

import (
	"goos/kernel/ipc"
	"testing"
)

func TestAdminPartsRespectIPCBound(t *testing.T) {
	var message ipc.Message
	if EncodeAdminPart(AdminStatus, AdminOK, make([]byte, MaxStatus+1), false, 1, &message) {
		t.Fatal("oversized part")
	}
	if !EncodeAdminPart(AdminStatus, AdminOK, make([]byte, MaxStatus), true, 1, &message) || message.PayloadSize != ipc.MaxPayload {
		t.Fatal("max part")
	}
	if _, text, more, ok := DecodeAdminPart(&message, AdminStatus, 1); !ok || !more || len(text) != MaxStatus {
		t.Fatal("part decode")
	}
	if _, _, ok := DecodeAdminReply(&message, AdminStatus, 1); ok {
		t.Fatal("multipart mistaken for complete reply")
	}
	message.PayloadSize = 65535
	if _, _, _, ok := DecodeAdminPart(&message, AdminStatus, 1); ok {
		t.Fatal("unbounded payload")
	}
}
func TestAdminPrivateChannelCodec(t *testing.T) {
	var message ipc.Message
	if !EncodeAdminRequest(AdminReload, 5, &message) {
		t.Fatal("encode")
	}
	if op, ok := DecodeAdminRequest(&message); !ok || op != AdminReload {
		t.Fatal("decode")
	}
	message.HandleCount = 1
	message.Handles[0] = 7
	if _, ok := DecodeAdminRequest(&message); ok {
		t.Fatal("request carried authority")
	}
	if !EncodeAdminReply(AdminStatus, AdminOK, []byte("listener=closed\n"), 5, &message) {
		t.Fatal("reply")
	}
	if result, text, ok := DecodeAdminReply(&message, AdminStatus, 5); !ok || result != AdminOK || string(text) != "listener=closed\n" {
		t.Fatal("reply decode")
	}
	if _, _, ok := DecodeAdminReply(&message, AdminStatus, 6); ok {
		t.Fatal("stale reply")
	}
	notice, ok := AdminSessionNotice(9)
	if !ok {
		t.Fatal("notice")
	}
	if channel, ok := DecodeAdminNotice(&notice); !ok || channel != 9 {
		t.Fatal("notice decode")
	}
	notice.HandleCount = 2
	if _, ok := DecodeAdminNotice(&notice); ok {
		t.Fatal("extra handles")
	}
	counts, ok := AdminCountsNotice(2, 4)
	if !ok {
		t.Fatal("counts")
	}
	if p, a, ok := DecodeCounts(&counts); !ok || p != 2 || a != 4 {
		t.Fatal("counts decode")
	}
	if _, ok := AdminCountsNotice(3, 4); ok {
		t.Fatal("unbounded pending")
	}
}
