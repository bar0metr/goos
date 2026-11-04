package linuxsock

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
)

// FuzzService feeds a family arbitrary messages and netd arbitrary answers.
// The service must never panic, and once the family is removed nothing of
// it may remain.
func FuzzService(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 1, 0, 0, 0}, []byte{0, 1, 2})
	f.Add([]byte{5, 0, 0, 0, 0, 0, 0, 0, 1}, []byte{3, 3, 3, 3})
	f.Fuzz(func(t *testing.T, script []byte, answers []byte) {
		h := newHarness(t)
		if len(answers) > 0 && answers[0]&1 != 0 {
			h.svc.SetWarm(int(answers[0]) % 4)
		}
		family := h.family(1, 7)
		rings := h.rings(family)
		for len(script) >= 8 && h.svc.Family(family) {
			step := script[:8]
			script = script[8:]
			request := linuxsockapi.Request{Opcode: linuxsockapi.Opcode(step[0] % 20), Flags: step[1] & 7, Slot: step[2] % 9,
				Length: uint32(step[3]) * 17, Address: [4]byte{step[4], 0, 0, 1}, Port: uint16(step[5]), Level: uint32(step[6] % 7),
				Name: uint32(step[7] % 9), Value: uint64(step[6])}
			if step[4]&1 == 0 && len(h.svc.sockets) > 0 {
				for index := range h.svc.sockets {
					if sock := &h.svc.sockets[index]; sock.used && !sock.opening && int(step[5])%3 == 0 {
						request.Socket = sock.id
						break
					}
				}
			}
			if request.Socket == 0 && request.Opcode != linuxsockapi.OpOpen && request.Opcode != linuxsockapi.OpCancel {
				request.Socket = linuxsockapi.MakeSocket(7, 1, int(step[5]%32))
			}
			// Some steps commit or hand back slots on the submit ring instead.
			if step[0]%23 == 22 {
				entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingKind(step[1]%2 + 1), Slot: step[2] % 9, Length: uint32(step[3]) * 70,
					Socket: request.Socket, Generation: uint32(step[6]), Flags: step[7] & 1}
				if entry.Kind == linuxsockapi.RingRelease {
					entry.Length, entry.Flags, entry.Socket = 0, 0, 0
				}
				linuxsockapi.PushSubmit(rings, &entry)
			}
			correlation := uint64(step[0]) + 1
			if request.Opcode == linuxsockapi.OpDoorbell || request.Opcode == linuxsockapi.OpReadyChanged {
				correlation = 0
			}
			var message ipc.Message
			if linuxsockapi.EncodeRequest(&request, correlation, &message) {
				if h.svc.CanReceive(family) {
					h.svc.Receive(family, &message)
				}
			} else {
				message = ipc.Message{ProtocolID: linuxsockapi.ProtocolID, Version: linuxsockapi.Version, Kind: ipc.KindRequest,
					Correlation: 1, PayloadSize: linuxsockapi.RequestSize}
				copy(message.Payload[:], step)
				h.svc.Receive(family, &message)
			}
			for len(answers) > 0 && len(h.backend.log) > 0 {
				entry := h.backend.log[0]
				h.backend.log = h.backend.log[1:]
				a := answers[0]
				answers = answers[1:]
				h.complete(entry, NativeCompletion{Result: NativeResult(a % 20), Endpoint: uint32(a) + 1,
					Mask: NativeMask(a) * 3, Readable: uint32(a), Length: uint32(a) * 9, Full: uint32(a) * 11,
					Value: uint32(a % 20), Data: make([]byte, int(a)*13)})
			}
			if len(answers) == 0 {
				h.transport.full[family] = !h.transport.full[family]
			}
			h.svc.Flush(family)
		}
		h.svc.RemoveFamily(family)
		// netd answers whatever is still parked.
		for len(h.backend.log) > 0 {
			entry := h.backend.log[0]
			h.backend.log = h.backend.log[1:]
			h.complete(entry, NativeCompletion{Result: NativeClosed})
		}
		h.svc.BackendDown(TCP, h.generation[TCP])
		h.svc.BackendDown(UDP, h.generation[UDP])
		for index := range h.svc.sockets {
			if h.svc.sockets[index].used {
				t.Fatal("socket left after removal")
			}
		}
		for index := range h.svc.ops {
			if h.svc.ops[index].state != opFree {
				t.Fatalf("op left after removal: %+v", h.svc.ops[index])
			}
		}
		if h.svc.submits != 0 || h.svc.waits != 0 {
			t.Fatalf("counts left: submits %d waits %d", h.svc.submits, h.svc.waits)
		}
	})
}
