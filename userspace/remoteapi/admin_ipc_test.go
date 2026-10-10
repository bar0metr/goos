package remoteapi

import (
	"goos/kernel/ipc"
	"testing"
)

func TestAdminMultipartKeepsRequestPendingUntilFinalReply(t *testing.T) {
	channel := ipc.NewChannel()
	var request ipc.Message
	if !EncodeAdminRequest(AdminStatus, 7, &request) || channel.Send(0, request, nil, nil, nil) != ipc.StatusOK {
		t.Fatal("request")
	}
	if _, status := channel.Receive(1); status != ipc.StatusOK {
		t.Fatal("receive request", status)
	}
	for part := 0; part < MaxStatusParts; part++ {
		more := part < MaxStatusParts-1
		var message ipc.Message
		if !EncodeAdminPart(AdminStatus, AdminOK, []byte{byte(part)}, more, 7, &message) {
			t.Fatal("encode part", part)
		}
		if status := channel.Send(1, message, nil, nil, nil); status != ipc.StatusOK {
			t.Fatalf("part %d rejected by IPC: %d", part, status)
		}
		wantPending := 1
		if !more {
			wantPending = 0
		}
		if channel.Pending(0) != wantPending {
			t.Fatal("request completed before final part", part)
		}
		incoming, status := channel.Receive(0)
		if status != ipc.StatusOK {
			t.Fatal("receive part", status)
		}
		result, text, remaining, ok := DecodeAdminPart(&incoming, AdminStatus, 7)
		if !ok || result != AdminOK || remaining != more || len(text) != 1 || text[0] != byte(part) {
			t.Fatal("decode part", part)
		}
	}
	if channel.Send(1, ipc.Message{ProtocolID: AdminProtocolID, Version: AdminVersion, Kind: ipc.KindReply, Correlation: 7}, nil, nil, nil) != ipc.StatusNotFound {
		t.Fatal("final reply did not complete request")
	}
}

func TestAdminPartRejectsWrongEnvelope(t *testing.T) {
	for _, more := range []bool{false, true} {
		var valid ipc.Message
		if !EncodeAdminPart(AdminStatus, AdminOK, []byte("status"), more, 9, &valid) {
			t.Fatal("encode")
		}
		if _, _, _, ok := DecodeAdminPart(&valid, AdminStatus, 8); ok {
			t.Fatal("wrong request accepted")
		}
		for _, mutate := range []func(*ipc.Message){
			func(m *ipc.Message) { m.Correlation++ },
			func(m *ipc.Message) { m.Payload[4] ^= 1 },
			func(m *ipc.Message) { m.Version-- },
			func(m *ipc.Message) { m.Kind = ipc.KindRequest },
		} {
			message := valid
			mutate(&message)
			if _, _, _, ok := DecodeAdminPart(&message, AdminStatus, 9); ok {
				t.Fatal("invalid envelope accepted", more)
			}
		}
	}
}
