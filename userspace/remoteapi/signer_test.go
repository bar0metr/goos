package remoteapi

import (
	"goos/kernel/ipc"
	"testing"
)

func TestSignProtocolRoundTripAndMalformed(t *testing.T) {
	request := SignRequest{Generation: 7}
	request.Data[0] = 9
	var message ipc.Message
	if !EncodeSignRequest(&request, 1, &message) {
		t.Fatal("encode request")
	}
	var got SignRequest
	if !DecodeSignRequest(&message, &got) || got != request {
		t.Fatal("request round trip")
	}
	changes := []func(*ipc.Message){
		func(m *ipc.Message) { m.ProtocolID++ }, func(m *ipc.Message) { m.Version++ }, func(m *ipc.Message) { m.Kind = ipc.KindReply }, func(m *ipc.Message) { m.Correlation = 0 }, func(m *ipc.Message) { m.PayloadSize-- }, func(m *ipc.Message) { m.PayloadSize++ }, func(m *ipc.Message) { m.Payload[0]++ }, func(m *ipc.Message) { m.Payload[1] = 1 }, func(m *ipc.Message) { clear(m.Payload[8:16]) }, func(m *ipc.Message) { m.HandleCount = 1; m.Handles[0] = 3 },
	}
	for i, change := range changes {
		bad := message
		change(&bad)
		before := got
		if DecodeSignRequest(&bad, &got) || got != before {
			t.Fatal("malformed request", i)
		}
	}
	reply := SignReply{Generation: 7, Result: ResultOK}
	reply.Signature[0] = 11
	if !EncodeSignReply(&reply, 1, &message) {
		t.Fatal("encode reply")
	}
	var decoded SignReply
	if !DecodeSignReply(&message, 1, 7, &decoded) || decoded != reply {
		t.Fatal("reply roundtrip")
	}
	changes = []func(*ipc.Message){func(m *ipc.Message) { m.ProtocolID++ }, func(m *ipc.Message) { m.Version++ }, func(m *ipc.Message) { m.Kind = ipc.KindRequest }, func(m *ipc.Message) { m.Correlation++ }, func(m *ipc.Message) { m.PayloadSize-- }, func(m *ipc.Message) { m.PayloadSize++ }, func(m *ipc.Message) { m.Payload[0]++ }, func(m *ipc.Message) { m.Payload[1] = 255 }, func(m *ipc.Message) { m.Payload[2] = 1 }, func(m *ipc.Message) { m.Payload[8]++ }, func(m *ipc.Message) { m.HandleCount = 1; m.Handles[0] = 3 }}
	for i, change := range changes {
		bad := message
		change(&bad)
		before := decoded
		if DecodeSignReply(&bad, 1, 7, &decoded) || decoded != before {
			t.Fatal("malformed reply", i)
		}
	}
	if DecodeSignReply(&message, 0, 7, &decoded) || DecodeSignReply(&message, 1, 0, &decoded) {
		t.Fatal("zero expected identity")
	}
	reply.Result = ResultUnavailable
	if EncodeSignReply(&reply, 1, &message) {
		t.Fatal("failure leaks signature")
	}
	clear(reply.Signature[:])
	if !EncodeSignReply(&reply, 1, &message) || !DecodeSignReply(&message, 1, 7, &decoded) {
		t.Fatal("failure reply")
	}
	message.Payload[16] = 1
	if DecodeSignReply(&message, 1, 7, &decoded) {
		t.Fatal("noncanonical failure signature")
	}
	if EncodeSignRequest(nil, 1, &message) || EncodeSignRequest(&request, 0, &message) || EncodeSignReply(nil, 1, &message) {
		t.Fatal("invalid encoder")
	}
}
