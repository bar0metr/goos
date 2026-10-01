package exec

import (
	"goos/kernel/ipc"
	"testing"
)

func TestControlRequestRoundTrip(t *testing.T) {
	message, ok := EncodeControl(ControlReboot, 0, 7)
	operation, argument, decoded := DecodeControl(message)
	if !ok || !decoded || operation != ControlReboot || argument != 0 || !IsControl(message) {
		t.Fatalf("operation %d %d %v %v", operation, argument, ok, decoded)
	}
	kill, ok := EncodeControl(ControlKill, Slots-1, 8)
	operation, argument, decoded = DecodeControl(kill)
	if !ok || !decoded || operation != ControlKill || argument != Slots-1 {
		t.Fatalf("kill = %d %d %v %v", operation, argument, ok, decoded)
	}
	for _, bad := range []struct {
		operation ControlOperation
		argument  uint8
	}{{ControlOperation(9), 0}, {ControlReboot, 1}, {ControlKill, Slots}, {ControlDiagnostics, 1}, {ControlFault, 0}, {ControlFault, FaultStorage + 1}} {
		if _, ok := EncodeControl(bad.operation, bad.argument, 7); ok {
			t.Fatalf("encoded %#v", bad)
		}
	}
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Payload[0] = 9 },
		func(m *ipc.Message) { m.Payload[1] = 1 },
		func(m *ipc.Message) { m.PayloadSize = 1 },
	} {
		corrupt := message
		mutate(&corrupt)
		if _, _, ok := DecodeControl(corrupt); ok {
			t.Fatal("decoded malformed control request")
		}
	}
	if _, _, ok := DecodeSubmit(message); ok {
		t.Fatal("control message decoded as a launch request")
	}
	for _, request := range []struct {
		operation ControlOperation
		argument  uint8
	}{{ControlDiagnostics, 0}, {ControlFault, FaultTerminal}, {ControlFault, FaultShell}, {ControlFault, FaultKernelStack}, {ControlFault, FaultFilesystem}, {ControlFault, FaultVFS}, {ControlFault, FaultConfig}, {ControlFault, FaultLog}, {ControlFault, FaultBlock}, {ControlFault, FaultDriver}, {ControlFault, FaultStorage}} {
		encoded, ok := EncodeControl(request.operation, request.argument, 11)
		operation, argument, decoded := DecodeControl(encoded)
		if !ok || !decoded || operation != request.operation || argument != request.argument {
			t.Fatalf("%d %d did not round trip", request.operation, request.argument)
		}
	}
	// A version 2 request is refused rather than read with the new meanings.
	old := message
	old.Version = 2
	if _, _, ok := DecodeControl(old); ok {
		t.Fatal("version 2 control request accepted")
	}
}

func TestServicesReplyRoundTripAndBounds(t *testing.T) {
	entries := []ServiceEntry{{Name: []byte("vfs"), State: 3, PID: 4}, {Name: []byte("virtio-blk"), State: 5, Restarts: 2, PID: 6}}
	message, ok := EncodeServices(entries, 9)
	var decoded [MaxServiceEntries]ServiceEntry
	count, valid := DecodeServices(message, &decoded)
	if !ok || !valid || count != 2 || string(decoded[1].Name) != "virtio-blk" || decoded[1].Restarts != 2 || decoded[1].PID != 6 {
		t.Fatalf("decoded %d %+v", count, decoded[:count])
	}
	if int(message.PayloadSize) > ipc.MaxPayload {
		t.Fatal("reply exceeds payload")
	}
	message.Payload[0] = MaxServiceEntries + 1
	if _, ok := DecodeServices(message, &decoded); ok {
		t.Fatal("oversized count accepted")
	}
	if _, ok := EncodeServices([]ServiceEntry{{Name: []byte("a-name-that-is-too-long")}}, 9); ok {
		t.Fatal("long name encoded")
	}
	full := make([]ServiceEntry, MaxServiceEntries)
	for index := range full {
		full[index] = ServiceEntry{Name: []byte("service-name-15")}
	}
	if message, ok := EncodeServices(full, 9); !ok || int(message.PayloadSize) > ipc.MaxPayload {
		t.Fatal("maximum reply does not fit")
	}
}
