// Package tcpring is the client of GTCP 4. Stream data goes through the
// session's rings: Write fills TX slots and queues them, Read posts RX
// credit and takes the slots netd fills. The channel carries lifecycle
// requests and OpDoorbell notifications only. The package holds no handle
// or mapping code; a Transport moves the channel messages, so host tests
// run the same client against tcpserve.Session4.
//
// A Client belongs to one goroutine at a time.
package tcpring

import (
	"goos/kernel/ipc"
	"goos/userspace/shmring"
	"goos/userspace/tcpapi"
)

// Transport moves one session's channel messages.
type Transport interface {
	// Send sends a request or a doorbell. It reports false when netd is
	// gone.
	Send(message *ipc.Message) bool
	// Receive takes netd's next message. With wait it blocks until one
	// arrives; without it, got is false when none waits. alive is false
	// once netd is gone.
	Receive(wait bool, message *ipc.Message) (got, alive bool)
	// Yield lets netd run before the client arms its doorbell.
	Yield()
}

const (
	// Spins is how many times a waiting client yields and polls its rings
	// before it arms its doorbell and sleeps.
	Spins = 8
	// ReadDepth is the credit a reading Conn keeps posted.
	ReadDepth = 4
)

// Stats count a client's channel traffic and ring entries.
type Stats struct {
	Requests, Replies, DoorbellsSent, DoorbellsReceived  uint64
	Writes, WritesDone, Data, Releases, Credits, Forgets uint64
}

// Client is one GTCP 4 session.
type Client struct {
	t           Transport
	tx, rx      []byte
	control     tcpapi.Control
	correlation uint64
	out, in     ipc.Message
	request     tcpapi.Request
	txGen       [tcpapi.TXSlots4]uint32
	// txLent: TX slots netd holds. rxHeld: RX slots the client holds.
	txLent  uint32
	rxGen   [tcpapi.RXSlots4]uint32
	rxHeld  uint32
	conns   [tcpapi.Endpoints4]*Conn
	accepts []*Conn
	// forgets: closed endpoints whose EntryForget waits for ring room.
	forgets []*Conn
	broken  bool
	// why names the first protocol rule the session broke.
	why   string
	Stats Stats
}

// Conn is one endpoint of the session.
type Conn struct {
	c     *Client
	index uint16
	id    uint32
	// listener is the listening Conn of a child accepted ahead.
	listener *Conn
	// A listener's accepts ahead: want children kept coming, granted of
	// them asked for and not arrived yet.
	want, granted uint8
	// offset is the stream offset of the next byte the client queues.
	offset   uint32
	inflight int
	err      tcpapi.Result
	credit   uint32
	pieces   [tcpapi.RXSlots4]piece
	first    int
	count    int
	ended    bool
	end      tcpapi.Result
	closing  bool
	gone     bool
	// Remote and Local are the connection's tuple, when it has one.
	Remote, Local         [4]byte
	RemotePort, LocalPort uint16
}

type piece struct {
	slot     uint8
	length   uint32
	consumed uint32
}

// New starts a client over the session's pools: tx with its control page
// writable, rx readable. It reports false when the pools are short or netd
// has not set the session up.
func New(t Transport, tx, rx []byte) (*Client, bool) {
	control, ok := tcpapi.ControlOf(tx, rx)
	if !ok || !control.ServiceReady() {
		return nil, false
	}
	return &Client{t: t, tx: tx, rx: rx, control: control}, true
}

// Call sends one channel request and waits for its reply, taking ring
// entries whenever a doorbell arrives meanwhile.
func (c *Client) Call(request *tcpapi.Request, reply *tcpapi.Reply) bool {
	if c.broken {
		return false
	}
	c.correlation++
	sent := c.correlation
	// The client's own copy keeps the caller's request on its stack.
	c.request = *request
	if !tcpapi.Layout4.EncodeRequest(&c.request, sent, &c.out) || !c.t.Send(&c.out) {
		return c.fail("request not sent")
	}
	c.Stats.Requests++
	for {
		got, alive := c.t.Receive(true, &c.in)
		if !alive || !got {
			return c.fail("no reply")
		}
		if c.in.Kind == ipc.KindNotification {
			if !tcpapi.IsDoorbell4(&c.in) {
				return c.fail("notification not a doorbell")
			}
			c.Stats.DoorbellsReceived++
			if _, ok := c.poll(); !ok {
				return false
			}
			continue
		}
		correlation, ok := tcpapi.Layout4.DecodeReply(&c.in, reply)
		if !ok || correlation != sent || reply.Opcode != request.Opcode {
			return c.fail("reply does not match the request")
		}
		c.Stats.Replies++
		return true
	}
}

func (c *Client) fail(why string) bool {
	if !c.broken {
		c.broken, c.why = true, why
	}
	return false
}

// Broken reports whether the session failed.
func (c *Client) Broken() bool { return c.broken }

// Why names what failed the session, or is empty.
func (c *Client) Why() string { return c.why }

// notify sends OpDoorbell when netd armed since the client last rang.
func (c *Client) notify() bool {
	if !c.control.ClientShouldRing() {
		return true
	}
	if !tcpapi.EncodeDoorbell4(&c.out) || !c.t.Send(&c.out) {
		return c.fail("doorbell not sent")
	}
	c.Stats.DoorbellsSent++
	return true
}

// Poll takes the ring entries netd published, without waiting.
func (c *Client) Poll() bool {
	_, ok := c.poll()
	return ok
}

// Wait blocks until a ring entry arrives: it polls across Spins yields,
// then arms the doorbell, checks once more and sleeps on the channel.
func (c *Client) Wait() bool {
	if c.broken {
		return false
	}
	for range Spins {
		if moved, ok := c.poll(); !ok || moved {
			return ok
		}
		c.t.Yield()
	}
	c.control.ClientArm()
	for {
		if moved, ok := c.poll(); !ok || moved {
			return ok
		}
		got, alive := c.t.Receive(true, &c.in)
		if !alive || !got {
			return c.fail("no doorbell")
		}
		if c.in.Kind != ipc.KindNotification || !tcpapi.IsDoorbell4(&c.in) {
			// No request is outstanding, so no reply can come.
			return c.fail("message while no request was out")
		}
		c.Stats.DoorbellsReceived++
	}
}

// poll takes every entry of the TXDone and RX rings. It reports whether it
// took any, and false in ok when netd broke the protocol.
func (c *Client) poll() (moved, ok bool) {
	var e tcpapi.Entry
	for {
		status := c.control.PopTXDone(&e)
		if status == shmring.Broken {
			return false, c.fail("TXDone ring broken")
		}
		if status != shmring.OK {
			break
		}
		moved = true
		if why := c.txDone(&e); why != "" {
			return false, c.fail(why)
		}
	}
	for {
		status := c.control.PopRX(&e)
		if status == shmring.Broken {
			return false, c.fail("RX ring broken")
		}
		if status != shmring.OK {
			break
		}
		moved = true
		if why := c.received(&e); why != "" {
			return false, c.fail(why)
		}
	}
	if !c.flushForgets() {
		return false, false
	}
	return moved, true
}

func (c *Client) conn(index uint16, id uint32) *Conn {
	conn := c.conns[index]
	if conn == nil || conn.id != id {
		return nil
	}
	return conn
}

// txDone takes one TXDone entry. It returns the rule the entry broke, or
// an empty string.
func (c *Client) txDone(e *tcpapi.Entry) string {
	if e.Kind == tcpapi.EntryClosed {
		conn := c.conn(e.Index, e.Endpoint)
		if conn == nil || !conn.closing {
			return "EntryClosed for an endpoint not closing"
		}
		c.forgets = append(c.forgets, conn)
		return ""
	}
	bit := uint32(1) << e.Slot
	if c.txLent&bit == 0 || c.txGen[e.Slot] != e.Generation {
		return "EntryWriteDone for a slot netd does not hold"
	}
	c.txLent &^= bit
	c.Stats.WritesDone++
	conn := c.conn(e.Index, e.Endpoint)
	if conn == nil {
		return "EntryWriteDone for an unknown endpoint"
	}
	conn.inflight--
	if e.Result != tcpapi.ResultOK && conn.err == tcpapi.ResultOK {
		conn.err = e.Result
	}
	return ""
}

// received takes one RX entry. It returns the rule the entry broke, or an
// empty string.
func (c *Client) received(e *tcpapi.Entry) string {
	switch e.Kind {
	case tcpapi.EntryAccepted:
		listener := c.conns[e.Offset]
		if listener == nil || listener.id != e.Generation {
			return "EntryAccepted for an unknown listener"
		}
		if c.conns[e.Index] != nil {
			return "EntryAccepted at an index in use"
		}
		child := &Conn{c: c, index: e.Index, id: e.Endpoint, listener: listener, Remote: e.Address, RemotePort: e.Port,
			Local: e.Local, LocalPort: e.LocalPort}
		if listener.granted != 0 {
			listener.granted--
		}
		c.conns[e.Index] = child
		c.accepts = append(c.accepts, child)
		return ""
	case tcpapi.EntryData:
		bit := uint32(1) << e.Slot
		if c.rxHeld&bit != 0 || e.Generation != c.rxGen[e.Slot]+1 {
			return "EntryData for a slot out of turn"
		}
		c.rxHeld |= bit
		c.rxGen[e.Slot] = e.Generation
		c.Stats.Data++
	}
	conn := c.conn(e.Index, e.Endpoint)
	if conn == nil {
		return "RX entry for an unknown endpoint"
	}
	if conn.credit == 0 {
		return "RX entry without credit"
	}
	conn.credit--
	if e.Kind == tcpapi.EntryEnd {
		if !conn.ended {
			conn.ended, conn.end = true, e.Result
		}
		return ""
	}
	if conn.closing {
		if !c.release(e.Slot) {
			return c.why
		}
		return ""
	}
	if conn.count == len(conn.pieces) {
		return "EntryData past the pieces a connection keeps"
	}
	conn.pieces[(conn.first+conn.count)%len(conn.pieces)] = piece{slot: e.Slot, length: e.Length}
	conn.count++
	return ""
}

// pushRelease puts an entry on the Release ring, waiting for room.
func (c *Client) pushRelease(e *tcpapi.Entry) bool {
	for {
		switch c.control.PushRelease(e) {
		case shmring.OK:
			return true
		case shmring.Broken:
			return c.fail("Release ring broken")
		}
		if !c.notify() {
			return false
		}
		c.t.Yield()
	}
}

func (c *Client) release(slot uint8) bool {
	c.rxHeld &^= 1 << slot
	c.Stats.Releases++
	return c.pushRelease(&tcpapi.Entry{Kind: tcpapi.EntryRelease, Slot: slot, Generation: c.rxGen[slot]})
}

// flushForgets gives up the closed endpoints netd announced, in one batch
// of EntryForget entries and at most one doorbell.
func (c *Client) flushForgets() bool {
	if len(c.forgets) == 0 {
		return true
	}
	for len(c.forgets) != 0 && c.control.ReleaseRoom() != 0 {
		conn := c.forgets[0]
		if c.control.PushRelease(&tcpapi.Entry{Kind: tcpapi.EntryForget, Index: conn.index, Endpoint: conn.id}) != shmring.OK {
			return c.fail("EntryForget not pushed")
		}
		c.forgets = c.forgets[1:]
		c.conns[conn.index] = nil
		conn.gone = true
		c.Stats.Forgets++
	}
	return c.notify()
}

func (c *Client) adopt(reply *tcpapi.Reply) *Conn {
	conn := &Conn{c: c, index: reply.Index, id: reply.Endpoint, Remote: reply.Address, RemotePort: reply.Port, Local: reply.Local,
		LocalPort: reply.LocalPort}
	if c.conns[reply.Index] != nil {
		c.fail("reply names an index in use")
		return nil
	}
	c.conns[reply.Index] = conn
	return conn
}

// Open opens an endpoint.
func (c *Client) Open() (*Conn, tcpapi.Result) {
	var reply tcpapi.Reply
	if !c.Call(&tcpapi.Request{Opcode: tcpapi.OpOpen}, &reply) {
		return nil, tcpapi.ResultDown
	}
	if reply.Result != tcpapi.ResultOK {
		return nil, reply.Result
	}
	return c.adopt(&reply), tcpapi.ResultOK
}

// Dial opens an endpoint and connects it in one request. With noWait it
// returns once the handshake started, with ResultInProgress.
func (c *Client) Dial(address [4]byte, port uint16, noWait bool) (*Conn, tcpapi.Result) {
	request := tcpapi.Request{Opcode: tcpapi.OpOpenConnect, Address: address, Port: port}
	if noWait {
		request.Flags = tcpapi.FlagNoWait
	}
	var reply tcpapi.Reply
	if !c.Call(&request, &reply) {
		return nil, tcpapi.ResultDown
	}
	if reply.Endpoint == 0 {
		return nil, reply.Result
	}
	return c.adopt(&reply), reply.Result
}

// Listen opens an endpoint, binds it and listens.
func (c *Client) Listen(address [4]byte, port uint16, backlog uint8) (*Conn, tcpapi.Result) {
	conn, result := c.Open()
	if result != tcpapi.ResultOK {
		return nil, result
	}
	var reply tcpapi.Reply
	for _, request := range [...]tcpapi.Request{{Opcode: tcpapi.OpBind, Endpoint: conn.id, Address: address, Port: port},
		{Opcode: tcpapi.OpListen, Endpoint: conn.id, Backlog: backlog}} {
		if !c.Call(&request, &reply) {
			return nil, tcpapi.ResultDown
		}
		if reply.Result != tcpapi.ResultOK {
			conn.Close()
			return nil, reply.Result
		}
		if request.Opcode == tcpapi.OpBind {
			conn.Local, conn.LocalPort = reply.Address, reply.Port
		}
	}
	return conn, tcpapi.ResultOK
}

// AcceptAhead lets netd accept up to count children of the listener
// without a request each. netd stops after count children; Accept asks for
// more once every child granted arrived, so the listener keeps count
// children coming.
func (l *Conn) AcceptAhead(count uint8) tcpapi.Result {
	result := l.askAhead(count)
	if result == tcpapi.ResultOK {
		l.want = count
	}
	return result
}

func (l *Conn) askAhead(count uint8) tcpapi.Result {
	var reply tcpapi.Reply
	if !l.c.Call(&tcpapi.Request{Opcode: tcpapi.OpAcceptAhead, Endpoint: l.id, Backlog: count}, &reply) {
		return tcpapi.ResultDown
	}
	if reply.Result == tcpapi.ResultOK {
		l.granted = count
	}
	return reply.Result
}

// topUp asks netd to accept ahead again once every child it was granted
// arrived and fewer than want wait for Accept. A refusal stops the
// accepts ahead; only a broken session fails the caller.
func (l *Conn) topUp() tcpapi.Result {
	if l.want == 0 || l.granted != 0 || l.closing {
		return tcpapi.ResultOK
	}
	waiting := uint8(0)
	for _, child := range l.c.accepts {
		if child.listener == l {
			waiting++
		}
	}
	if waiting >= l.want {
		return tcpapi.ResultOK
	}
	if result := l.askAhead(l.want - waiting); result != tcpapi.ResultOK {
		l.want = 0
		if result == tcpapi.ResultDown {
			return result
		}
	}
	return tcpapi.ResultOK
}

// Accept returns the next child of the listener: one netd accepted ahead,
// or, without accept-ahead, one an OpAccept request returns.
func (l *Conn) Accept(ahead bool) (*Conn, tcpapi.Result) {
	c := l.c
	if !ahead {
		var reply tcpapi.Reply
		if !c.Call(&tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: l.id}, &reply) {
			return nil, tcpapi.ResultDown
		}
		if reply.Result != tcpapi.ResultOK {
			return nil, reply.Result
		}
		return c.adopt(&reply), tcpapi.ResultOK
	}
	for {
		for index, child := range c.accepts {
			if child.listener == l {
				c.accepts = append(c.accepts[:index], c.accepts[index+1:]...)
				if result := l.topUp(); result != tcpapi.ResultOK {
					return nil, result
				}
				return child, tcpapi.ResultOK
			}
		}
		if result := l.topUp(); result != tcpapi.ResultOK {
			return nil, result
		}
		if !c.Wait() {
			return nil, tcpapi.ResultDown
		}
	}
}

// Endpoint is the netd endpoint ID.
func (conn *Conn) Endpoint() uint32 { return conn.id }

// Gone reports whether netd destroyed the closed endpoint and the client
// gave its ID up.
func (conn *Conn) Gone() bool { return conn.gone }

func (c *Client) freeTX() (uint8, bool) {
	for slot := uint8(0); slot < tcpapi.TXSlots4; slot++ {
		if c.txLent&(1<<slot) == 0 {
			return slot, true
		}
	}
	return 0, false
}

// Write queues p in TX slots. It returns once netd holds every byte; Flush
// waits for the stack to take them. A transport error a finished slot
// reported ends the write with that error.
func (conn *Conn) Write(p []byte) (int, tcpapi.Result) { return conn.write(p, true) }

// TryWrite queues what fits in the free TX slots now. It returns
// ResultWouldBlock when none was free.
func (conn *Conn) TryWrite(p []byte) (int, tcpapi.Result) { return conn.write(p, false) }

func (conn *Conn) write(p []byte, block bool) (int, tcpapi.Result) {
	c := conn.c
	written := 0
	if _, ok := c.poll(); !ok {
		return 0, tcpapi.ResultDown
	}
	for written < len(p) {
		if conn.err != tcpapi.ResultOK {
			return written, conn.err
		}
		if conn.closing {
			return written, tcpapi.ResultClosed
		}
		slot, free := c.freeTX()
		if !free && !block {
			if !c.notify() {
				return written, tcpapi.ResultDown
			}
			if written == 0 {
				return 0, tcpapi.ResultWouldBlock
			}
			return written, tcpapi.ResultOK
		}
		if !free {
			if !c.notify() || !c.Wait() {
				return written, tcpapi.ResultDown
			}
			continue
		}
		base := tcpapi.Layout4.Offset(slot)
		length := copy(c.tx[base:base+tcpapi.SlotSize4], p[written:])
		c.txGen[slot]++
		e := tcpapi.Entry{Kind: tcpapi.EntryWrite, Slot: slot, Index: conn.index, Endpoint: conn.id, Length: uint32(length),
			Generation: c.txGen[slot], Offset: conn.offset}
		if c.control.PushTX(&e) != shmring.OK {
			// The client holds at most TXSlots4 entries there.
			c.fail("EntryWrite not pushed")
			return written, tcpapi.ResultDown
		}
		c.txLent |= 1 << slot
		conn.offset += uint32(length)
		conn.inflight++
		written += length
		c.Stats.Writes++
	}
	if !c.notify() {
		return written, tcpapi.ResultDown
	}
	return written, tcpapi.ResultOK
}

// Flush waits until netd handed back every TX slot of the connection and
// returns the first error one carried.
func (conn *Conn) Flush() tcpapi.Result {
	c := conn.c
	for conn.inflight != 0 {
		if !c.notify() || !c.Wait() {
			return tcpapi.ResultDown
		}
	}
	return conn.err
}

// post keeps ReadDepth fills posted, and no more than the pieces the
// connection has room for: a reader behind its fills would otherwise have
// more arrive than it can keep.
func (conn *Conn) post() bool {
	room := uint32(len(conn.pieces) - conn.count)
	if conn.ended || conn.closing || conn.credit >= min(ReadDepth, room) {
		return true
	}
	more := min(ReadDepth, room) - conn.credit
	conn.credit += more
	conn.c.Stats.Credits++
	return conn.c.pushRelease(&tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: conn.index, Endpoint: conn.id, Length: more})
}

// Read copies arrived bytes into p. It blocks until some arrive, and
// returns ResultEOF, or the error that ended the stream, once the stream's
// bytes ran out.
func (conn *Conn) Read(p []byte) (int, tcpapi.Result) { return conn.read(p, true) }

// TryRead copies bytes that already arrived, or returns ResultWouldBlock.
func (conn *Conn) TryRead(p []byte) (int, tcpapi.Result) { return conn.read(p, false) }

func (conn *Conn) read(p []byte, block bool) (int, tcpapi.Result) {
	c := conn.c
	if _, ok := c.poll(); !ok {
		return 0, tcpapi.ResultDown
	}
	for {
		if conn.count != 0 {
			head := &conn.pieces[conn.first]
			base := tcpapi.Layout4.Offset(head.slot) + uint64(head.consumed)
			read := copy(p, c.rx[base:base+uint64(head.length-head.consumed)])
			head.consumed += uint32(read)
			if head.consumed == head.length {
				conn.first = (conn.first + 1) % len(conn.pieces)
				conn.count--
				if !c.release(head.slot) || !conn.post() || !c.notify() {
					return read, tcpapi.ResultDown
				}
			}
			return read, tcpapi.ResultOK
		}
		if conn.ended {
			return 0, conn.end
		}
		if conn.closing {
			return 0, tcpapi.ResultClosed
		}
		if !conn.post() || !c.notify() {
			return 0, tcpapi.ResultDown
		}
		if !block {
			return 0, tcpapi.ResultWouldBlock
		}
		if !c.Wait() {
			return 0, tcpapi.ResultDown
		}
	}
}

// ShutdownWrite sends the FIN after the bytes queued before it.
func (conn *Conn) ShutdownWrite() tcpapi.Result {
	var reply tcpapi.Reply
	if !conn.c.Call(&tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: conn.id}, &reply) {
		return tcpapi.ResultDown
	}
	return reply.Result
}

// Readiness reads the endpoint's readiness entry without a request.
func (conn *Conn) Readiness() (tcpapi.Readiness, bool) {
	r, ok := conn.c.control.LoadReadiness(int(conn.index))
	return r, ok && r.Endpoint == conn.id
}

// Close hands the endpoint to netd. netd still sends the bytes queued
// before it, hands their slots back and announces the endpoint's end; the
// client then gives the ID up. RX slots the endpoint held go back now.
func (conn *Conn) Close() tcpapi.Result {
	c := conn.c
	if conn.closing {
		return tcpapi.ResultInvalidArgument
	}
	// netd may close the endpoint and publish EntryClosed before the client
	// reads the reply: a doorbell queued ahead of the reply makes Call take
	// the rings. The endpoint counts as closing from the request on.
	conn.closing = true
	var reply tcpapi.Reply
	if !c.Call(&tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: conn.id}, &reply) {
		return tcpapi.ResultDown
	}
	if reply.Result != tcpapi.ResultOK {
		conn.closing = false
		return reply.Result
	}
	for ; conn.count != 0; conn.count-- {
		if !c.release(conn.pieces[conn.first].slot) {
			return tcpapi.ResultDown
		}
		conn.first = (conn.first + 1) % len(conn.pieces)
	}
	if !c.notify() {
		return tcpapi.ResultDown
	}
	return tcpapi.ResultOK
}

// Settle waits until every closed endpoint is gone and every TX slot is
// back, so a benchmark can check the session ended clean.
func (c *Client) Settle() bool {
	for {
		pending := c.txLent != 0
		for _, conn := range c.conns {
			if conn != nil && conn.closing {
				pending = true
			}
		}
		if !pending {
			return true
		}
		if !c.Wait() {
			return false
		}
	}
}

// Held reports the TX slots netd holds and the RX slots the client holds.
func (c *Client) Held() (tx, rx uint32) { return c.txLent, c.rxHeld }

// WaitReady blocks until the endpoint's mask differs from seen, for a
// client that must sleep on a readiness change no ring entry brings.
func (conn *Conn) WaitReady(seen uint16) (uint16, tcpapi.Result) {
	var reply tcpapi.Reply
	if !conn.c.Call(&tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: conn.id, Mask: seen}, &reply) {
		return 0, tcpapi.ResultDown
	}
	return reply.Mask, reply.Result
}
