package tcpring

import (
	"bytes"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
)

// streamResult is one in-process stream: its channel messages in total and
// in the steady middle half, and the bytes that half moved.
type streamResult struct {
	messages, steady, steadyBytes, slots int
}

func (r streamResult) perWindow(messages int) float64 {
	return float64(messages) * 65536 / float64(r.steadyBytes)
}

// streamV4 sends total bytes from one GTCP 4 client to another over
// loopback, one goroutine driving both clients and netd's loop in turn. It
// checks every byte and that every TX slot came back without an error.
func streamV4(t testing.TB, total int) streamResult {
	n := newNetd(t)
	sendWire, recvWire, sender, receiver, out, in := connected(t, n, 7100)
	count := func() int { return sendWire.messages() + recvWire.messages() }
	data := pattern(0, total)
	got := make([]byte, 0, total)
	buffer := make([]byte, 1<<16)
	var result streamResult
	start, mark, closed := count(), -1, false
	sent, writes := 0, sender.Stats.Writes
	for len(got) < total {
		if sent < total {
			written, r := out.TryWrite(data[sent:min(total, sent+1<<16)])
			if r != tcpapi.ResultOK && r != tcpapi.ResultWouldBlock {
				t.Fatalf("write %d", r)
			}
			sent += written
		}
		read, r := in.TryRead(buffer)
		if r != tcpapi.ResultOK && r != tcpapi.ResultWouldBlock {
			t.Fatalf("read %d", r)
		}
		got = append(got, buffer[:read]...)
		switch {
		case mark < 0 && len(got) >= total/4:
			mark, result.steadyBytes = count(), len(got)
		case !closed && mark >= 0 && len(got) >= 3*total/4:
			result.steady, result.steadyBytes, closed = count()-mark, len(got)-result.steadyBytes, true
		}
		n.mu.Lock()
		n.step()
		n.mu.Unlock()
		if sender.Broken() || receiver.Broken() {
			t.Fatalf("session broken: %q %q", sendWire.fault(), recvWire.fault())
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatal("stream differs")
	}
	if r := out.Flush(); r != tcpapi.ResultOK {
		t.Fatalf("flush %d", r)
	}
	if tx, _ := sender.Held(); tx != 0 || sender.Stats.WritesDone != sender.Stats.Writes {
		t.Fatalf("TX slots out %b, %d of %d done", tx, sender.Stats.WritesDone, sender.Stats.Writes)
	}
	result.messages = count() - start
	result.slots = int(sender.Stats.Writes - writes)
	return result
}

// v3wire collects a GTCP 3 session's replies through the codec and counts
// them.
type v3wire struct {
	sent     []struct{ reply tcpapi.Reply }
	replies  int
	requests int
}

func (w *v3wire) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !tcpapi.Layout3.EncodeReply(reply, correlation, &message) {
		return false
	}
	w.replies++
	w.sent = append(w.sent, struct{ reply tcpapi.Reply }{*reply})
	return true
}

// streamV3 sends total bytes between two GTCP 3 sessions the way
// linuxsockd does: NoWait writes at the accepted offset, NoWait reads that
// return the last slot in their Release field.
func streamV3(t testing.TB, total int) streamResult {
	stack := new(network.Stack)
	stack.Init(77)
	now := uint64(1_000_000_000)
	var server, client tcpserve.Session
	pools := func() ([]byte, []byte) {
		return make([]byte, tcpapi.Layout3.TXPool()), make([]byte, tcpapi.Layout3.RXPool())
	}
	serverTX, serverRX := pools()
	clientTX, clientRX := pools()
	if !server.StartLayout(stack, 0, 0, tcpapi.Layout3, serverTX, serverRX, now) || !client.StartLayout(stack, 1, 0, tcpapi.Layout3, clientTX, clientRX, now) {
		t.Fatal("start")
	}
	w := new(v3wire)
	correlation := uint64(0)
	call := func(s *tcpserve.Session, request tcpapi.Request) tcpapi.Reply {
		t.Helper()
		correlation++
		w.requests++
		w.sent = w.sent[:0]
		if fault := s.Handle(stack, &request, correlation, now, w); fault != tcpserve.FaultNone {
			t.Fatalf("fault %q", fault)
		}
		if len(w.sent) != 1 {
			t.Fatalf("opcode %d: %d replies", request.Opcode, len(w.sent))
		}
		return w.sent[0].reply
	}
	listener := call(&server, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	call(&server, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: loopback, Port: 7101})
	call(&server, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 4})
	out := call(&client, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	call(&client, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: out, Address: loopback, Port: 7101, Flags: tcpapi.FlagNoWait})
	in := call(&server, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener, Flags: tcpapi.FlagNoWait}).Endpoint
	start := w.requests + w.replies
	data := pattern(0, total)
	got := make([]byte, 0, total)
	accepted, release := 0, uint8(0)
	var result streamResult
	mark, closed := -1, false
	for len(got) < total {
		for slot := uint8(0); slot < tcpapi.PipelinedWrites3 && accepted < total; slot++ {
			length := min(total-accepted, tcpapi.SlotSize3)
			base := tcpapi.Layout3.Offset(slot)
			copy(clientTX[base:], data[accepted:accepted+length])
			reply := call(&client, tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: out, Slot: slot, Length: uint16(length),
				Flags: tcpapi.FlagNoWait, Offset: uint32(accepted)})
			accepted += int(reply.Length)
			result.slots++
			if int(reply.Length) < length {
				break
			}
		}
		for {
			reply := call(&server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: in, Length: tcpapi.SlotSize3,
				Flags: tcpapi.FlagNoWait, Release: release})
			release = 0
			if reply.Result != tcpapi.ResultOK {
				break
			}
			base := tcpapi.Layout3.Offset(reply.Slot)
			got = append(got, serverRX[base:base+uint64(reply.Length)]...)
			release = 1 << reply.Slot
		}
		switch {
		case mark < 0 && len(got) >= total/4:
			mark, result.steadyBytes = w.requests+w.replies, len(got)
		case !closed && mark >= 0 && len(got) >= 3*total/4:
			result.steady, result.steadyBytes, closed = w.requests+w.replies-mark, len(got)-result.steadyBytes, true
		}
		for _, s := range []*tcpserve.Session{&server, &client} {
			if fault, _ := s.Deliver(stack, now, w); fault != tcpserve.FaultNone {
				t.Fatalf("deliver %q", fault)
			}
		}
		now += 1_000_000
		stack.Advance(now)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("GTCP 3 stream differs")
	}
	result.messages = w.requests + w.replies - start
	return result
}

// TestStreamMessageCounts is the in-process comparison: the channel
// messages per 64 KiB of stream, GTCP 3 against GTCP 4. GTCP 4 must move
// the steady middle half of the stream with no channel message at all.
func TestStreamMessageCounts(t *testing.T) {
	const total = 8 << 20
	v3, v4 := streamV3(t, total), streamV4(t, total)
	t.Logf("GTCP 3: %.1f messages per 64 KiB steady, %d in all for %d slots", v3.perWindow(v3.steady), v3.messages, v3.slots)
	t.Logf("GTCP 4: %.1f messages per 64 KiB steady, %d in all for %d slots", v4.perWindow(v4.steady), v4.messages, v4.slots)
	if v4.steady != 0 {
		t.Fatalf("GTCP 4 sent %d channel messages in the steady state", v4.steady)
	}
	if v3.steady == 0 || v4.steadyBytes < total/4 || v3.steadyBytes < total/4 {
		t.Fatal("the steady window measured nothing")
	}
}

func benchmarkStream(b *testing.B, run func(testing.TB, int) streamResult) {
	const total = 4 << 20
	b.SetBytes(total)
	var last streamResult
	for b.Loop() {
		last = run(b, total)
	}
	b.ReportMetric(float64(last.messages)*65536/total, "msgs/64KiB")
	b.ReportMetric(last.perWindow(last.steady), "steady-msgs/64KiB")
}

func BenchmarkStreamGTCP3(b *testing.B) { benchmarkStream(b, streamV3) }
func BenchmarkStreamGTCP4(b *testing.B) { benchmarkStream(b, streamV4) }
