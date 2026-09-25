package config

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

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
		func(m *ipc.Message) { m.ProtocolID++ },
	} {
		message := notice
		damage(&message)
		if _, ok := DecodeSessionNotice(&message); ok {
			t.Fatalf("damaged notice accepted: %+v", message)
		}
	}
	if _, _, _, ok := DecodeAddSession(notice); ok {
		t.Fatal("a notice taken for a session request")
	}
}
