package tcpring

import (
	"bytes"
	"sync"
	"testing"

	"goos/userspace/tcpapi"
)

// connected opens a listener on one session that accepts ahead, dials it
// from another and returns both ends.
func connected(t testing.TB, n *netd, port uint16) (sendWire, recvWire *wire, sender, receiver *Client, out, in *Conn) {
	t.Helper()
	recvWire, receiver = n.session()
	sendWire, sender = n.session()
	listener, result := receiver.Listen(loopback, port, 4)
	if result != tcpapi.ResultOK {
		t.Fatalf("listen %d", result)
	}
	if result := listener.AcceptAhead(4); result != tcpapi.ResultOK {
		t.Fatalf("accept ahead %d", result)
	}
	out, result = sender.Dial(loopback, port, false)
	if result != tcpapi.ResultOK {
		t.Fatalf("dial %d", result)
	}
	in, result = listener.Accept(true)
	if result != tcpapi.ResultOK || in.RemotePort != out.LocalPort || in.LocalPort != port {
		t.Fatalf("accept %d %+v", result, in)
	}
	return
}

func TestStreamBothWaysAndClose(t *testing.T) {
	n := newNetd(t)
	sendWire, recvWire, sender, receiver, out, in := connected(t, n, 7001)
	const total = 1 << 20
	setup := sendWire.messages() + recvWire.messages()
	before := sendWire.requests + recvWire.requests
	var wg sync.WaitGroup
	var got []byte
	var readResult tcpapi.Result
	wg.Add(1)
	go func() {
		defer wg.Done()
		buffer := make([]byte, 5000)
		for {
			read, result := in.Read(buffer)
			got = append(got, buffer[:read]...)
			if result != tcpapi.ResultOK {
				readResult = result
				return
			}
		}
	}()
	data := pattern(0, total)
	for offset := 0; offset < total; {
		chunk := min(total-offset, 40000)
		written, result := out.Write(data[offset : offset+chunk])
		if result != tcpapi.ResultOK || written != chunk {
			t.Fatalf("write %d %d", written, result)
		}
		offset += chunk
	}
	if result := out.Flush(); result != tcpapi.ResultOK {
		t.Fatalf("flush %d", result)
	}
	// Data moved without a request or a reply.
	if requests := sendWire.requests + recvWire.requests - before; requests != 0 {
		t.Fatalf("%d requests during the stream", requests)
	}
	if result := out.ShutdownWrite(); result != tcpapi.ResultOK {
		t.Fatalf("shutdown %d", result)
	}
	wg.Wait()
	if readResult != tcpapi.ResultEOF || !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, result %d", len(got), readResult)
	}
	for _, c := range []struct {
		client *Client
		conn   *Conn
	}{{sender, out}, {receiver, in}} {
		if result := c.conn.Close(); result != tcpapi.ResultOK {
			t.Fatalf("close %d", result)
		}
		if !c.client.Settle() || !c.conn.Gone() {
			t.Fatal("close did not finish")
		}
		if tx, rx := c.client.Held(); tx != 0 || rx != 0 {
			t.Fatalf("slots held tx %b rx %b", tx, rx)
		}
	}
	for _, w := range []*wire{sendWire, recvWire} {
		if w.fault() != "" || w.server.TXHeld() != 0 || w.server.RXLent() != 0 {
			t.Fatalf("server fault %q tx %b rx %b", w.fault(), w.server.TXHeld(), w.server.RXLent())
		}
	}
	t.Logf("setup %d messages; stream of %d KiB: sender %d requests %d doorbells, receiver %d requests %d doorbells",
		setup, total>>10, sendWire.requests, sendWire.doorbellsIn+sendWire.doorbellsOut, recvWire.requests, recvWire.doorbellsIn+recvWire.doorbellsOut)
}

func TestDialRefusedLeavesNoEndpoint(t *testing.T) {
	n := newNetd(t)
	_, c := n.session()
	conn, result := c.Dial(loopback, 7999, false)
	if conn != nil || result == tcpapi.ResultOK {
		t.Fatalf("dial to nothing %d", result)
	}
	conn, result = c.Dial(loopback, 7999, true)
	if result != tcpapi.ResultInProgress || conn == nil {
		t.Fatalf("NoWait dial %d", result)
	}
	ready, _ := conn.Readiness()
	for ready.Mask&tcpapi.ReadyConnectFailed == 0 {
		mask, result := conn.WaitReady(ready.Mask)
		if result != tcpapi.ResultOK {
			t.Fatalf("wait ready %d", result)
		}
		ready.Mask = mask
	}
	if conn.Close() != tcpapi.ResultOK || !c.Settle() {
		t.Fatal("close")
	}
}

func TestReadinessPageWithoutRequests(t *testing.T) {
	n := newNetd(t)
	sendWire, recvWire, _, receiver, out, in := connected(t, n, 7002)
	before := sendWire.requests + recvWire.requests
	if written, result := out.Write(pattern(0, 3000)); written != 3000 || result != tcpapi.ResultOK || out.Flush() != tcpapi.ResultOK {
		t.Fatal("write")
	}
	n.mu.Lock()
	n.step()
	n.mu.Unlock()
	ready, ok := in.Readiness()
	if !ok || ready.Mask&tcpapi.ReadyReadable == 0 || ready.Readable != 3000 {
		t.Fatalf("readiness %+v %v", ready, ok)
	}
	if sendWire.requests+recvWire.requests != before {
		t.Fatal("readiness cost a request")
	}
	buffer := make([]byte, 4000)
	if read, result := in.Read(buffer); read != 3000 || result != tcpapi.ResultOK || !bytes.Equal(buffer[:read], pattern(0, 3000)) {
		t.Fatalf("read %d %d", read, result)
	}
	_ = receiver
}

func TestCloseDrainsQueuedWrites(t *testing.T) {
	n := newNetd(t)
	_, _, sender, _, out, in := connected(t, n, 7003)
	const total = 200 << 10
	if written, result := out.Write(pattern(0, total)); written != total || result != tcpapi.ResultOK {
		t.Fatal("write")
	}
	// Close while netd still holds slots: they come back, then the end.
	if result := out.Close(); result != tcpapi.ResultOK {
		t.Fatalf("close %d", result)
	}
	if _, result := out.Write([]byte{1}); result != tcpapi.ResultClosed {
		t.Fatalf("write after close %d", result)
	}
	var got []byte
	buffer := make([]byte, 1<<16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			read, result := in.Read(buffer)
			got = append(got, buffer[:read]...)
			if result != tcpapi.ResultOK {
				return
			}
		}
	}()
	if !sender.Settle() || !out.Gone() {
		t.Fatal("sender did not settle")
	}
	<-done
	if !bytes.Equal(got, pattern(0, total)) {
		t.Fatalf("got %d of %d bytes", len(got), total)
	}
}

// TestAcceptAheadKeepsChildrenComing accepts more children than one
// OpAcceptAhead grants, on one session, as netbench4 does. netd stops after
// the grant, so Accept has to ask again or it waits until the session dies.
func TestAcceptAheadKeepsChildrenComing(t *testing.T) {
	n := newNetd(t)
	_, c := n.session()
	listener, result := c.Listen(loopback, 7009, 8)
	if result != tcpapi.ResultOK {
		t.Fatalf("listen %d", result)
	}
	if result := listener.AcceptAhead(2); result != tcpapi.ResultOK {
		t.Fatalf("accept ahead %d", result)
	}
	conns := make([]*Conn, 0, 20)
	for index := 0; index < 10; index++ {
		client, result := c.Dial(loopback, 7009, false)
		if result != tcpapi.ResultOK {
			t.Fatalf("dial %d: %d", index, result)
		}
		server, result := listener.Accept(true)
		if result != tcpapi.ResultOK || server.RemotePort != client.LocalPort {
			t.Fatalf("accept %d: %d", index, result)
		}
		for _, conn := range [...]*Conn{client, server} {
			if result := conn.Close(); result != tcpapi.ResultOK {
				t.Fatalf("close %d: %d", index, result)
			}
		}
		conns = append(conns, client, server)
	}
	if !c.Settle() {
		t.Fatal("settle")
	}
	for _, conn := range conns {
		if !conn.Gone() {
			t.Fatal("endpoint not released")
		}
	}
}
