package vfs

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func TestAddSessionControlRoundTrip(t *testing.T) {
	message, ok := AddSessionRequest(handle.ID(7), 9)
	if !ok || message.PayloadSize != 1 {
		t.Fatal("request encode failed")
	}
	server, ok := DecodeAddSession(message)
	if !ok || server != 7 {
		t.Fatalf("server = %d, %v", server, ok)
	}
	message.Payload[0] = 0
	if _, ok := DecodeAddSession(message); ok {
		t.Fatal("accepted invalid operation")
	}
}

func TestSetFilesystemControlRoundTrip(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		message, ok := SetFilesystemRequest(FilesystemAttach{Client: 11, Generation: 0x0102030405060708, Fenced: fenced}, 3)
		if !ok {
			t.Fatal("request encode failed")
		}
		attach, ok := DecodeSetFilesystem(message)
		if !ok || attach.Client != 11 || attach.Generation != 0x0102030405060708 || attach.Fenced != fenced {
			t.Fatalf("attach = %+v, %v", attach, ok)
		}
		if _, ok := DecodeAddSession(message); ok {
			t.Fatal("a filesystem endpoint was taken for a session")
		}
	}
	for _, attach := range []FilesystemAttach{{Generation: 1}, {Client: 11}} {
		if _, ok := SetFilesystemRequest(attach, 3); ok {
			t.Fatalf("encoded %+v", attach)
		}
	}
	if _, ok := SetFilesystemRequest(FilesystemAttach{Client: 11, Generation: 1}, 0); ok {
		t.Fatal("encoded an uncorrelated request")
	}
	message, _ := SetFilesystemRequest(FilesystemAttach{Client: 11, Generation: 4, Fenced: true}, 3)
	for name, damage := range map[string]func(*ipc.Message){
		"no endpoint":   func(m *ipc.Message) { m.HandleCount = 0 },
		"zero endpoint": func(m *ipc.Message) { m.Handles[0] = 0 },
		"two handles":   func(m *ipc.Message) { m.HandleCount, m.Handles[1] = 2, 12 },
		"old version":   func(m *ipc.Message) { m.Version-- },
		"old payload":   func(m *ipc.Message) { m.PayloadSize = 1 },
		"unknown flag":  func(m *ipc.Message) { m.Payload[1] = 2 },
		"zero gen":      func(m *ipc.Message) { clear(m.Payload[2:10]) },
		"notification":  func(m *ipc.Message) { m.Kind = ipc.KindNotification },
		"uncorrelated":  func(m *ipc.Message) { m.Correlation = 0 },
	} {
		corrupt := message
		damage(&corrupt)
		if _, ok := DecodeSetFilesystem(corrupt); ok {
			t.Fatalf("decoded a request with %s", name)
		}
	}
}

func TestFilesystemLostNoticeRoundTrip(t *testing.T) {
	notice, ok := FilesystemLostNotice(0x1122334455667788)
	if !ok || notice.Kind != ipc.KindNotification || notice.Correlation != 0 || notice.HandleCount != 0 {
		t.Fatalf("notice = %+v, %v", notice, ok)
	}
	if generation, ok := DecodeFilesystemLostNotice(&notice); !ok || generation != 0x1122334455667788 {
		t.Fatalf("decode = %#x, %v", generation, ok)
	}
	if _, ok := FilesystemLostNotice(0); ok {
		t.Fatal("a notice without an attachment generation")
	}
	for name, damage := range map[string]func(*ipc.Message){
		"request":     func(m *ipc.Message) { m.Kind = ipc.KindRequest },
		"reply":       func(m *ipc.Message) { m.Kind = ipc.KindReply },
		"correlated":  func(m *ipc.Message) { m.Correlation = 1 },
		"handle":      func(m *ipc.Message) { m.HandleCount, m.Handles[0] = 1, 5 },
		"operation":   func(m *ipc.Message) { m.Payload[0] = operationSession },
		"short":       func(m *ipc.Message) { m.PayloadSize = 1 },
		"zero gen":    func(m *ipc.Message) { clear(m.Payload[1:9]) },
		"old version": func(m *ipc.Message) { m.Version-- },
		"foreign":     func(m *ipc.Message) { m.ProtocolID++ },
	} {
		message := notice
		damage(&message)
		if _, ok := DecodeFilesystemLostNotice(&message); ok {
			t.Fatalf("damaged notice accepted: %s", name)
		}
	}
	// The notice is no session notice and no request the VFS serves.
	if _, ok := DecodeSessionNotice(&notice); ok {
		t.Fatal("a filesystem notice taken for a session notice")
	}
	if _, ok := DecodeSetFilesystem(notice); ok {
		t.Fatal("a filesystem notice taken for an attach")
	}
}

func TestUnmountControlRoundTrip(t *testing.T) {
	message, ok := UnmountRequest(7)
	if !ok || !IsUnmount(message) || IsSync(message) {
		t.Fatal("unmount request did not round trip")
	}
	message.HandleCount = 1
	if IsUnmount(message) {
		t.Fatal("unmount request accepted an attached handle")
	}
	if _, ok := UnmountRequest(0); ok {
		t.Fatal("encoded an uncorrelated unmount")
	}
}

func TestSessionNoticeRoundTrip(t *testing.T) {
	notice, ok := SessionNotice(handle.ID(0x100000009))
	if !ok {
		t.Fatal("encode")
	}
	if session, ok := DecodeSessionNotice(&notice); !ok || session != handle.ID(0x100000009) {
		t.Fatalf("decode = %#x, %v", session, ok)
	}
	if _, ok := SessionNotice(0); ok {
		t.Fatal("a notice without a session")
	}
	for _, damage := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Kind = ipc.KindRequest },
		func(m *ipc.Message) { m.Correlation = 1 },
		func(m *ipc.Message) { m.HandleCount = 0 },
		func(m *ipc.Message) { m.Payload[0] = operationAddSession },
		func(m *ipc.Message) { m.Version-- },
	} {
		message := notice
		damage(&message)
		if _, ok := DecodeSessionNotice(&message); ok {
			t.Fatalf("damaged notice accepted: %+v", message)
		}
	}
	// A session request is not a notice, and the other way round.
	request, _ := AddSessionRequest(handle.ID(0x100000009), 1)
	if _, ok := DecodeSessionNotice(&request); ok {
		t.Fatal("a session request taken for a notice")
	}
	if _, ok := DecodeAddSession(notice); ok {
		t.Fatal("a notice taken for a session request")
	}
}

func TestPublishAndWithdrawControlRoundTrip(t *testing.T) {
	publish, ok := PublishRequest(8)
	if !ok || !IsPublish(publish) || IsWithdraw(publish) || IsUnmount(publish) || IsSync(publish) {
		t.Fatalf("publish request = %#v, %v", publish, ok)
	}
	withdraw, ok := WithdrawRequest(9)
	if !ok || !IsWithdraw(withdraw) || IsPublish(withdraw) {
		t.Fatalf("withdraw request = %#v, %v", withdraw, ok)
	}
	withdraw.HandleCount = 1
	if IsWithdraw(withdraw) {
		t.Fatal("withdraw request carrying a handle was accepted")
	}
	if _, ok := PublishRequest(0); ok {
		t.Fatal("publish request without a correlation was encoded")
	}
}
