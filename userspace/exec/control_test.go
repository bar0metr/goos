package exec

import (
	"goos/kernel/ipc"
	"testing"
)

func TestControlRequestRoundTrip(t *testing.T) {
	message, ok := EncodeControl(ControlReboot, 7)
	operation, decoded := DecodeControl(message)
	if !ok || !decoded || operation != ControlReboot || !IsControl(message) {
		t.Fatalf("operation %d %v %v", operation, ok, decoded)
	}
	if _, ok := EncodeControl(ControlOperation(9), 7); ok {
		t.Fatal("unknown operation encoded")
	}
	message.Payload[0] = 9
	if _, ok := DecodeControl(message); ok {
		t.Fatal("unknown operation decoded")
	}
	if _, _, ok := DecodeSubmit(message); ok {
		t.Fatal("control message decoded as a launch request")
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
