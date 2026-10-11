package remoteapi

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func TestExecOpenBoundedCommandAndThreeStreams(t *testing.T) {
	for _, size := range []uint16{1, MaxCommand} {
		request := Request{Operation: OperationOpen, Mode: ModeExec, Command: 17, CommandSize: size}
		var message ipc.Message
		if !EncodeRequest(&request, 11, &message) || message.HandleCount != 1 || message.Handles[0] != 17 {
			t.Fatal("exec encode")
		}
		var decoded Request
		if !DecodeRequest(&message, &decoded) || decoded != request {
			t.Fatal("exec decode", decoded)
		}
		for _, mutate := range []func(*ipc.Message){
			func(m *ipc.Message) { m.HandleCount = 0 },
			func(m *ipc.Message) { m.HandleCount = 2; m.Handles[1] = 18 },
			func(m *ipc.Message) { m.Handles[0] = 0 },
			func(m *ipc.Message) { m.Payload[16] = 1 },
			func(m *ipc.Message) { m.Payload[2] = 0 },
			func(m *ipc.Message) { m.Payload[4] = 1; m.Payload[5] = 16 },
		} {
			bad := message
			mutate(&bad)
			if DecodeRequest(&bad, &decoded) {
				t.Fatal("malformed exec accepted", bad)
			}
		}
	}
	reply := Reply{Operation: OperationOpen, Mode: ModeExec, Result: ResultOK, Session: 9}
	var message ipc.Message
	if EncodeReply(&reply, []handle.ID{1, 2}, 11, &message) {
		t.Fatal("exec missing stderr accepted")
	}
	if !EncodeReply(&reply, []handle.ID{1, 2, 3}, 11, &message) {
		t.Fatal("exec reply encode")
	}
	var got Reply
	if !DecodeReply(&message, 11, OperationOpen, &got) || got != reply {
		t.Fatal("exec reply decode")
	}
	message.Handles[2] = message.Handles[1]
	if DecodeReply(&message, 11, OperationOpen, &got) {
		t.Fatal("aliased stderr accepted")
	}
}
