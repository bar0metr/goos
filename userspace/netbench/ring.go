//go:build goos_userspace

package netbench

import (
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpring"
	"goos/userspace/utility"
)

// RunRing measures a GTCP 4 session, whose stream data moves through shared
// rings: the stream, a one-byte request-reply and a connect-accept-close
// loop that opens with OpOpenConnect, accepts ahead and closes without
// waiting for the end. Its lines start with native-v4-, and each adds the
// channel messages and doorbells the step cost.
func RunRing(data startup.Data) {
	c, ok := utility.OpenTCPRing(data)
	if !ok {
		utility.Fail(nil, "GTCP 4 session not granted")
	}
	prefix, ringClient = "native-v4-", c
	total := uint64(32) << 20
	if data.ArgCount() == 1 {
		value, _ := data.Arg(1)
		mib, ok := decimal(value)
		if !ok || mib < 1 || mib > 4096 {
			utility.Usage()
		}
		total = mib << 20
	}
	listener, result := c.Listen(loopback, 0, 8)
	ringCheck("listen", result)
	ringCheck("accept ahead", listener.AcceptAhead(4))
	port := listener.LocalPort
	if port == 0 {
		utility.Fail([]byte("listen"), "no port")
	}
	client, result := c.Dial(loopback, port, false)
	ringCheck("dial", result)
	server, result := listener.Accept(true)
	ringCheck("accept", result)
	ringStream(c, client, server, total)
	ringRequestReply(c, client, server)
	ringCheck("close client", client.Close())
	ringCheck("close server", server.Close())
	ringConnectClose(c, listener, port)
	ringCheck("close listener", listener.Close())
	if !c.Settle() {
		ringCheck("settle", tcpapi.ResultDown)
	}
	if tx, rx := c.Held(); tx != 0 || rx != 0 {
		utility.Fail([]byte("settle"), "slots still out")
	}
	utility.PrintText("bench native ok")
	utility.EndLine()
}

// ringClient is the session RunRing measures; a failure names the rule
// it broke.
var ringClient *tcpring.Client

func ringCheck(what string, result tcpapi.Result) {
	if result == tcpapi.ResultOK {
		return
	}
	if ringClient != nil && ringClient.Why() != "" {
		utility.Fail([]byte(what), result.Text()+": "+ringClient.Why())
	}
	utility.Fail([]byte(what), result.Text())
}

// traffic is a snapshot of the client's channel counters.
type traffic struct{ messages, doorbells uint64 }

func ringTraffic(c *tcpring.Client) traffic {
	s := &c.Stats
	return traffic{messages: s.Requests + s.Replies + s.DoorbellsSent + s.DoorbellsReceived,
		doorbells: s.DoorbellsSent + s.DoorbellsReceived}
}

func (t traffic) since(before traffic) traffic {
	return traffic{messages: t.messages - before.messages, doorbells: t.doorbells - before.doorbells}
}

// ringStream writes total bytes from client to server, both ends driven by
// this goroutine: it queues what fits, takes what arrived, and sleeps on
// the doorbell only when neither moved. Every byte must arrive in order and
// every TX slot must come back without an error.
func ringStream(c *tcpring.Client, client, server *tcpring.Conn, total uint64) {
	var source [tcpapi.SlotSize4]byte
	for index := range source {
		source[index] = byte(index % 251)
	}
	var sent, received uint64
	before := ringTraffic(c)
	start := utility.ClockNanos()
	for received < total {
		moved := false
		if sent < total {
			offset := sent % 251
			length := min(total-sent, uint64(len(source))-offset)
			written, result := client.TryWrite(source[offset : offset+length])
			if result != tcpapi.ResultOK && result != tcpapi.ResultWouldBlock {
				ringCheck("stream write", result)
			}
			sent += uint64(written)
			moved = written != 0
		}
		read, result := server.TryRead(sink[:])
		if result != tcpapi.ResultOK && result != tcpapi.ResultWouldBlock {
			ringCheck("stream read", result)
		}
		for index := range read {
			if sink[index] != byte((received+uint64(index))%251) {
				utility.Fail([]byte("stream"), "bytes out of order")
			}
		}
		received += uint64(read)
		if !moved && read == 0 && !c.Wait() {
			ringCheck("stream", tcpapi.ResultDown)
		}
	}
	ringCheck("stream flush", client.Flush())
	elapsed := (utility.ClockNanos() - start) / 1000
	spent := ringTraffic(c).since(before)
	out := begin(prefix + "tcp-stream")
	out = field(out, "bytes", total)
	out = field(out, "us", elapsed)
	if elapsed != 0 {
		out = field(out, "MiBps", total*1000000/elapsed>>20)
	}
	out = field(out, "msgs-per-64k-x100", spent.messages*100*65536/total)
	finish(field(out, "bells-per-64k-x100", spent.doorbells*100*65536/total))
}

// ringRequestReply sends one byte each way per round.
func ringRequestReply(c *tcpring.Client, client, server *tcpring.Conn) {
	before := ringTraffic(c)
	start := utility.ClockNanos()
	for index := 0; index < rounds; index++ {
		ringExchange(client, server)
		ringExchange(server, client)
	}
	elapsed := (utility.ClockNanos() - start) / 1000
	spent := ringTraffic(c).since(before)
	out := begin(prefix + "tcp-request-reply")
	out = field(out, "rounds", rounds)
	out = field(out, "us-per-round", elapsed/rounds)
	finish(field(out, "msgs-per-round-x100", spent.messages*100/rounds))
}

func ringExchange(from, to *tcpring.Conn) {
	var one = [1]byte{'x'}
	if written, result := from.Write(one[:]); written != 1 || result != tcpapi.ResultOK {
		utility.Fail([]byte("write"), result.Text())
	}
	if read, result := to.Read(sink[:]); read != 1 || result != tcpapi.ResultOK {
		utility.Fail([]byte("read"), result.Text())
	}
}

// ringConnectClose dials, accepts ahead and closes connections one at a
// time. Closes do not wait for the connections to end; the loop then
// waits until netd announced every end and the client gave every ID up,
// so the count covers the whole lifecycle.
func ringConnectClose(c *tcpring.Client, listener *tcpring.Conn, port uint16) {
	var spent [4]uint64
	conns := make([]*tcpring.Conn, 0, 2*connects)
	before := ringTraffic(c)
	start := utility.ClockNanos()
	for index := 0; index < connects; index++ {
		mark := utility.ClockNanos()
		client, result := c.Dial(loopback, port, false)
		ringCheck("dial", result)
		now := utility.ClockNanos()
		spent[0] += now - mark
		mark = now
		server, result := listener.Accept(true)
		ringCheck("accept", result)
		now = utility.ClockNanos()
		spent[1] += now - mark
		mark = now
		ringCheck("close client", client.Close())
		now = utility.ClockNanos()
		spent[2] += now - mark
		mark = now
		ringCheck("close server", server.Close())
		spent[3] += utility.ClockNanos() - mark
		conns = append(conns, client, server)
	}
	if !c.Settle() {
		ringCheck("connect-close", tcpapi.ResultDown)
	}
	for _, conn := range conns {
		if !conn.Gone() {
			utility.Fail([]byte("connect-close"), "endpoint not released")
		}
	}
	elapsed := (utility.ClockNanos() - start) / 1000
	traffic := ringTraffic(c).since(before)
	out := begin(prefix + "tcp-connect-close")
	out = field(out, "count", connects)
	out = field(out, "us-per-connection", elapsed/connects)
	finish(field(out, "msgs-per-connection-x100", traffic.messages*100/connects))
	report(prefix+"tcp-open-connect", "count", connects, "us-per-op", spent[0]/1000/connects)
	report(prefix+"tcp-accept", "count", connects, "us-per-op", spent[1]/1000/connects)
	report(prefix+"tcp-close-client", "count", connects, "us-per-op", spent[2]/1000/connects)
	report(prefix+"tcp-close-server", "count", connects, "us-per-op", spent[3]/1000/connects)
}
