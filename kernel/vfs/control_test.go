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
	message, ok := SetFilesystemRequest(handle.ID(11), 3)
	if !ok {
		t.Fatal("request encode failed")
	}
	client, ok := DecodeSetFilesystem(message)
	if !ok || client != 11 {
		t.Fatalf("client = %d, %v", client, ok)
	}
	if _, ok := DecodeAddSession(message); ok {
		t.Fatal("a filesystem endpoint was taken for a session")
	}
	if _, ok := SetFilesystemRequest(0, 3); ok {
		t.Fatal("encoded a request with no endpoint")
	}
	corrupt := message
	corrupt.HandleCount = 0
	if _, ok := DecodeSetFilesystem(corrupt); ok {
		t.Fatal("decoded a request with no endpoint")
	}
	corrupt = message
	corrupt.Version--
	if _, ok := DecodeSetFilesystem(corrupt); ok {
		t.Fatal("decoded an older control version")
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
