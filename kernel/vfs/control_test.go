package vfs

import (
	"goos/kernel/handle"
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
