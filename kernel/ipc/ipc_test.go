package ipc

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"testing"
)

func message(kind Kind, correlation uint64) Message {
	value := Message{ProtocolID: 0x474f4f53, Version: 1, Kind: kind, Correlation: correlation}
	copy(value.Payload[:], []byte("ping"))
	value.PayloadSize = 4
	return value
}

func TestReceiveStatusDoesNotConsumeMessage(t *testing.T) {
	channel := NewChannel()
	if status := channel.ReceiveStatus(1); status != StatusNotFound {
		t.Fatalf("empty status = %d", status)
	}
	message := Message{ProtocolID: 1, Version: 1, Kind: KindNotification}
	if status := channel.Send(0, message, nil, nil, nil); status != StatusOK {
		t.Fatalf("send status = %d", status)
	}
	if status := channel.ReceiveStatus(1); status != StatusOK {
		t.Fatalf("ready status = %d", status)
	}
	if _, status := channel.Receive(1); status != StatusOK {
		t.Fatalf("receive status = %d", status)
	}
	if !channel.Close(0) || channel.ReceiveStatus(1) != StatusPeerDied {
		t.Fatal("peer close was not observable")
	}
}

func TestRequestReply(t *testing.T) {
	channel := NewChannel()
	if status := channel.Send(0, message(KindRequest, 7), nil, nil, nil); status != StatusOK {
		t.Fatalf("request status = %d", status)
	}
	request, status := channel.Receive(1)
	if status != StatusOK || request.Correlation != 7 || string(request.Payload[:request.PayloadSize]) != "ping" {
		t.Fatalf("unexpected request: %#v status=%d", request, status)
	}
	if status := channel.Send(1, message(KindReply, 7), nil, nil, nil); status != StatusOK {
		t.Fatalf("reply status = %d", status)
	}
	if _, status := channel.Receive(0); status != StatusOK {
		t.Fatalf("receive reply status = %d", status)
	}
	if status := channel.Send(1, message(KindReply, 7), nil, nil, nil); status != StatusNotFound {
		t.Fatalf("orphan reply status = %d", status)
	}
}

func TestQueueIsBounded(t *testing.T) {
	channel := NewChannel()
	for index := 0; index < QueueCapacity; index++ {
		if status := channel.Send(0, message(KindNotification, 0), nil, nil, nil); status != StatusOK {
			t.Fatalf("notification %d status = %d", index, status)
		}
	}
	if status := channel.Send(0, message(KindNotification, 0), nil, nil, nil); status != StatusResourceExhausted {
		t.Fatalf("full queue status = %d", status)
	}
}

func TestCorrelationIsUnique(t *testing.T) {
	channel := NewChannel()
	if channel.Send(0, message(KindRequest, 9), nil, nil, nil) != StatusOK ||
		channel.Send(0, message(KindRequest, 9), nil, nil, nil) != StatusAlreadyExists {
		t.Fatal("duplicate correlation accepted")
	}
}

func TestPeerDeathAfterQueueDrain(t *testing.T) {
	channel := NewChannel()
	channel.Send(0, message(KindNotification, 0), nil, nil, nil)
	if !channel.Close(0) {
		t.Fatal("close failed")
	}
	if _, status := channel.Receive(1); status != StatusOK {
		t.Fatalf("queued message lost: %d", status)
	}
	if _, status := channel.Receive(1); status != StatusPeerDied {
		t.Fatalf("peer death status = %d", status)
	}
}

func TestHandleTransferDerivesRights(t *testing.T) {
	channel := NewChannel()
	var source, target handle.Table
	sourceID, _ := source.Insert(handle.TypeMemory, capability.New(42, capability.Read|capability.Transfer))
	transfer := []Transfer{{Handle: sourceID, Type: handle.TypeMemory, Rights: capability.Read}}
	if status := channel.Send(0, message(KindNotification, 0), &source, &target, transfer); status != StatusOK {
		t.Fatalf("send status = %d", status)
	}
	received, status := channel.Receive(1)
	if status != StatusOK || received.HandleCount != 1 {
		t.Fatalf("missing transferred handle: %#v status=%d", received, status)
	}
	if _, ok := target.Lookup(received.Handles[0], handle.TypeMemory, capability.Read); !ok {
		t.Fatal("derived handle lacks read authority")
	}
	if _, ok := target.Lookup(received.Handles[0], handle.TypeMemory, capability.Write); ok {
		t.Fatal("transferred handle escalated rights")
	}
}

func TestHandleTransferRejectsMissingAuthority(t *testing.T) {
	channel := NewChannel()
	var source, target handle.Table
	sourceID, _ := source.Insert(handle.TypeMemory, capability.New(42, capability.Read))
	transfer := []Transfer{{Handle: sourceID, Type: handle.TypeMemory, Rights: capability.Read}}
	if status := channel.Send(0, message(KindNotification, 0), &source, &target, transfer); status != StatusPermissionDenied {
		t.Fatalf("unauthorized transfer status = %d", status)
	}
}

func TestHandleTransferRollsBackPartialDerivation(t *testing.T) {
	channel := NewChannel()
	var source, target handle.Table
	first, _ := source.Insert(handle.TypeMemory, capability.New(1, capability.Read|capability.Transfer))
	second, _ := source.Insert(handle.TypeMemory, capability.New(2, capability.Read))
	transfers := []Transfer{
		{Handle: first, Type: handle.TypeMemory, Rights: capability.Read},
		{Handle: second, Type: handle.TypeMemory, Rights: capability.Read},
	}
	if status := channel.Send(0, message(KindNotification, 0), &source, &target, transfers); status != StatusPermissionDenied {
		t.Fatalf("partial transfer status = %d", status)
	}
	if target.Available() != handle.MaxHandles {
		t.Fatal("partial transfer left a derived handle behind")
	}
	if _, ok := target.Insert(handle.TypeMemory, capability.New(999, capability.Read)); !ok {
		t.Fatal("partial transfer did not release derived handle")
	}
}

func TestOutstandingRequestsAreBounded(t *testing.T) {
	channel := NewChannel()
	for index := 0; index < QueueCapacity; index++ {
		request := message(KindRequest, uint64(index+1))
		if status := channel.Send(0, request, nil, nil, nil); status != StatusOK {
			t.Fatalf("request %d status = %d", index, status)
		}
	}
	for index := 0; index < QueueCapacity; index++ {
		channel.Receive(1)
	}
	if status := channel.Send(0, message(KindRequest, 99), nil, nil, nil); status != StatusResourceExhausted {
		t.Fatalf("pending request bound status = %d", status)
	}
}

func TestHandleTransferReportsReceiverExhaustion(t *testing.T) {
	channel := NewChannel()
	var source, target handle.Table
	sourceID, _ := source.Insert(handle.TypeMemory, capability.New(1, capability.Read|capability.Transfer))
	for index := 0; index < handle.MaxHandles; index++ {
		target.Insert(handle.TypeTask, capability.New(uint64(index+10), capability.Wait))
	}
	transfer := []Transfer{{Handle: sourceID, Type: handle.TypeMemory, Rights: capability.Read}}
	if status := channel.Send(0, message(KindNotification, 0), &source, &target, transfer); status != StatusResourceExhausted {
		t.Fatalf("receiver exhaustion status = %d", status)
	}
}

func TestInvalidMessages(t *testing.T) {
	channel := NewChannel()
	tests := []Message{
		{},
		{ProtocolID: 1, Version: 1, Kind: 99},
		{ProtocolID: 1, Version: 1, Kind: KindRequest},
		{ProtocolID: 1, Version: 1, Kind: KindNotification, Correlation: 1},
		{ProtocolID: 1, Version: 1, Kind: KindNotification, PayloadSize: MaxPayload + 1},
	}
	for index, value := range tests {
		if status := channel.Send(0, value, nil, nil, nil); status != StatusInvalidArgument {
			t.Errorf("invalid message %d status = %d", index, status)
		}
	}
}

func BenchmarkChannelPingPong(b *testing.B) {
	channel := NewChannel()
	request := message(KindNotification, 0)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		channel.Send(0, request, nil, nil, nil)
		channel.Receive(1)
	}
}
