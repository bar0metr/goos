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
	}{{ControlOperation(9), 0}, {ControlReboot, 1}, {ControlKill, Slots}, {ControlDiagnostics, 1}, {ControlFault, 0}, {ControlFault, FaultVolumeLast + 1}} {
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
	}{{ControlDiagnostics, 0}, {ControlFault, FaultTerminal}, {ControlFault, FaultShell}, {ControlFault, FaultKernelStack}, {ControlFault, FaultFilesystem}, {ControlFault, FaultVFS}, {ControlFault, FaultConfig}, {ControlFault, FaultLog}, {ControlFault, FaultBlock}, {ControlFault, FaultDriver}, {ControlFault, FaultStorage}, {ControlFault, FaultNetDriver}, {ControlFault, FaultRemoteSigner}, {ControlFault, FaultRemoteWorker}} {
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

func TestServicesListingStartIndex(t *testing.T) {
	for _, start := range []uint8{0, MaxServiceEntries, MaxServiceIndex - 1} {
		message, ok := EncodeControl(ControlServices, start, 7)
		if !ok {
			t.Fatalf("start %d not encoded", start)
		}
		if operation, argument, valid := DecodeControl(message); !valid || operation != ControlServices || argument != start {
			t.Fatalf("start %d decoded as %d %d %v", start, operation, argument, valid)
		}
	}
	if message, ok := EncodeControl(ControlServices, MaxServiceIndex, 7); ok {
		if _, _, valid := DecodeControl(message); valid {
			t.Fatal("start past the service table accepted")
		}
	}
}

func TestShutdownStatusRoundTrip(t *testing.T) {
	for _, status := range []ControlStatus{ControlSyncFailed, ControlResetFailed, ControlUnsupported, ControlAccepted} {
		message, ok := EncodeControlStatus(status, 9)
		decoded, valid := DecodeControlStatus(message)
		if !ok || !valid || decoded != status {
			t.Fatalf("status %d did not round trip", status)
		}
	}
	if _, ok := EncodeControlStatus(ControlAccepted+1, 9); ok {
		t.Fatal("unknown status encoded")
	}
}

func TestVolumeCommands(t *testing.T) {
	mount := VolumeCommand{Mount: true, Type: []byte("auto"), Source: []byte("name:data"), Path: []byte("/mnt/data")}
	message, ok := EncodeVolumeCommand(mount, 4)
	decoded, valid := DecodeVolumeCommand(&message)
	if !ok || !valid || !decoded.Mount || string(decoded.Type) != "auto" || string(decoded.Source) != "name:data" || string(decoded.Path) != "/mnt/data" {
		t.Fatalf("mount: %+v %v", decoded, valid)
	}
	if _, _, ok := DecodeControl(message); ok {
		t.Fatal("a mount decoded as a plain control")
	}
	unmount, ok := EncodeVolumeCommand(VolumeCommand{Path: []byte("/mnt/data")}, 5)
	decoded, valid = DecodeVolumeCommand(&unmount)
	if !ok || !valid || decoded.Mount || string(decoded.Path) != "/mnt/data" {
		t.Fatalf("unmount: %+v %v", decoded, valid)
	}
	trailing := unmount
	trailing.PayloadSize++
	if _, ok := DecodeVolumeCommand(&trailing); ok {
		t.Fatal("trailing byte accepted")
	}
	if _, ok := EncodeVolumeCommand(VolumeCommand{Mount: true, Path: []byte("/mnt/x")}, 5); ok {
		t.Fatal("mount without a source")
	}
	if _, ok := EncodeVolumeCommand(VolumeCommand{Mount: true, Source: make([]byte, MaxVolumeArgument+1), Path: []byte("/mnt/x")}, 5); ok {
		t.Fatal("oversized source")
	}
	reply, _ := EncodeVolumeReply(11, 13, 9)
	status, detail, ok := DecodeVolumeReply(reply)
	if !ok || status != 11 || detail != 13 {
		t.Fatalf("reply %d %d %v", status, detail, ok)
	}
	if _, ok := DecodeControlStatus(reply); ok {
		t.Fatal("a volume reply decoded as a control status")
	}
}

func TestWritableVolumeCommand(t *testing.T) {
	mount := VolumeCommand{Mount: true, Writable: true, Type: []byte("ext4"), Source: []byte("part:2"), Path: []byte("/mnt/data")}
	message, ok := EncodeVolumeCommand(mount, 9)
	decoded, valid := DecodeVolumeCommand(&message)
	if !ok || !valid || !decoded.Mount || !decoded.Writable || string(decoded.Source) != "part:2" {
		t.Fatalf("writable mount: %+v %v", decoded, valid)
	}
	plain := VolumeCommand{Mount: true, Source: []byte("part:2"), Path: []byte("/mnt/data")}
	message, _ = EncodeVolumeCommand(plain, 10)
	if decoded, _ := DecodeVolumeCommand(&message); decoded.Writable {
		t.Fatal("plain mount decoded writable")
	}
	if _, ok := EncodeVolumeCommand(VolumeCommand{Writable: true, Path: []byte("/mnt/data")}, 11); ok {
		t.Fatal("writable unmount encoded")
	}
}

// A version 4 mount is refused: version 5 gave the command a writable kind.
func TestVolumeCommandRefusesVersion4(t *testing.T) {
	message, ok := EncodeVolumeCommand(VolumeCommand{Mount: true, Writable: true, Source: []byte("part:2"), Path: []byte("/mnt/data")}, 3)
	if !ok || message.Version != ControlVersion {
		t.Fatalf("encoded version %d", message.Version)
	}
	message.Version = 4
	if _, ok := DecodeVolumeCommand(&message); ok {
		t.Fatal("version 4 mount accepted")
	}
}

// A version 5 request is refused: version 6 added the sync operation.
func TestSyncRefusesVersion5(t *testing.T) {
	message, ok := EncodeControl(ControlSync, 0, 4)
	if operation, _, decoded := DecodeControl(message); !ok || !decoded || operation != ControlSync {
		t.Fatal("sync did not round trip")
	}
	if _, _, decoded := DecodeControl(func() ipc.Message { old := message; old.Version = 5; return old }()); decoded {
		t.Fatal("version 5 sync accepted")
	}
	if _, _, decoded := DecodeControl(func() ipc.Message { bad := message; bad.Payload[1] = 1; return bad }()); decoded {
		t.Fatal("sync with an argument accepted")
	}
}
