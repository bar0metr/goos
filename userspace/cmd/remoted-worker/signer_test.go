package main

import (
	"crypto/ed25519"
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/remoteapi"
	"testing"
)

type signerFixture struct {
	key                        ed25519.PrivateKey
	sent                       ipc.Message
	sends, closed, attachments int
	time                       uint64
	status                     kcall.Status
	mutate                     func(*ipc.Message)
	afterReceive               func()
}

func (f *signerFixture) client(t *testing.T) *hostSigner {
	t.Helper()
	f.key = ed25519.NewKeyFromSeed(make([]byte, 32))
	f.time = 1
	s, ok := newHostSigner(f.key.Public().(ed25519.PublicKey), 7, signerTransport{
		send: func(m *ipc.Message) bool { f.sent = *m; f.sends++; return true },
		receive: func(ticks uint64, m *ipc.Message) kcall.Status {
			if ticks > 5*clock.TicksPerSecond {
				t.Fatal("unbounded signing deadline")
			}
			if f.status != kcall.StatusOK {
				return f.status
			}
			var request remoteapi.SignRequest
			if !remoteapi.DecodeSignRequest(&f.sent, &request) {
				t.Fatal("request")
			}
			reply := remoteapi.SignReply{Generation: request.Generation, Result: remoteapi.ResultOK}
			copy(reply.Signature[:], ed25519.Sign(f.key, request.Data[:]))
			if !remoteapi.EncodeSignReply(&reply, f.sent.Correlation, m) {
				t.Fatal("reply")
			}
			if f.mutate != nil {
				f.mutate(m)
			}
			if f.afterReceive != nil {
				f.afterReceive()
			}
			return kcall.StatusOK
		}, closeHandles: func(m *ipc.Message) { f.attachments += int(m.HandleCount) }, close: func() { f.closed++ }, now: func() uint64 { return f.time },
	})
	if !ok {
		t.Fatal("client init")
	}
	return s
}
func TestSignerExactDataSequence(t *testing.T) {
	var f signerFixture
	s := f.client(t)
	data := make([]byte, 32)
	data[0] = 9
	for _, correlation := range []uint64{1, 2} {
		sig, err := s.Sign(data)
		if err != nil || !ed25519.Verify(s.public[:], data, sig) || f.sent.Correlation != correlation || f.sent.Payload[16] != 9 {
			t.Fatal("signing contract", err)
		}
	}
	if f.closed != 0 || s.inFlight {
		t.Fatal("successful signer closed")
	}
}
func TestSignerFailuresArePermanent(t *testing.T) {
	for _, name := range []string{"peer", "timeout", "correlation", "generation", "handles", "signature", "deadline", "failed-result", "replay", "overflow", "input", "expired", "send", "reentrant", "clock-overflow"} {
		t.Run(name, func(t *testing.T) {
			var f signerFixture
			s := f.client(t)
			data := make([]byte, 32)
			switch name {
			case "peer":
				f.status = kcall.StatusPeerDied
			case "timeout":
				f.status = kcall.StatusTimeout
			case "correlation":
				f.mutate = func(m *ipc.Message) { m.Correlation++ }
			case "generation":
				f.mutate = func(m *ipc.Message) { m.Payload[8]++ }
			case "handles":
				f.mutate = func(m *ipc.Message) { m.HandleCount = 1; m.Handles[0] = 3 }
			case "signature":
				f.mutate = func(m *ipc.Message) { m.Payload[16]++ }
			case "deadline":
				f.afterReceive = func() { f.time += 5 * clock.TicksPerSecond }
			case "failed-result":
				f.mutate = func(m *ipc.Message) { m.Payload[1] = byte(remoteapi.ResultUnavailable); clear(m.Payload[16:80]) }
			case "replay":
				if _, err := s.Sign(data); err != nil {
					t.Fatal(err)
				}
				f.mutate = func(m *ipc.Message) { m.Correlation = 1 }
			case "overflow":
				s.sequence = ^uint64(0)
			case "input":
				data = data[:31]
			case "expired":
				s.deadline = f.time
			case "send":
				s.transport.send = func(*ipc.Message) bool { return false }
			case "reentrant":
				s.transport.send = func(*ipc.Message) bool { s.Sign(data); return true }
			case "clock-overflow":
				f.time = ^uint64(0) - 1
			}
			if _, err := s.Sign(data); err != errHostSigner || !s.failed || f.closed != 1 || s.inFlight {
				t.Fatal("failure state", err)
			}
			sends := f.sends
			if _, err := s.Sign(make([]byte, 32)); err != errHostSigner || f.sends != sends || f.closed != 1 {
				t.Fatal("failed signer reused")
			}
			if name == "handles" && f.attachments != 1 {
				t.Fatal("attached handle leaked")
			}
		})
	}
}

func TestSignerHandshakeDeadlineIsNotExtended(t *testing.T) {
	var f signerFixture
	s := f.client(t)
	s.deadline = f.time + 2
	receive := s.transport.receive
	s.transport.receive = func(ticks uint64, m *ipc.Message) kcall.Status {
		if ticks != 2 {
			t.Fatal("handshake budget extended", ticks)
		}
		return receive(ticks, m)
	}
	if _, err := s.Sign(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
}
