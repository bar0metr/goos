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
