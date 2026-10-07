// Package upstream carries the resolver's DNS queries over a GUDP1 session
// (ADR 0195). It holds the per-query state the resolver service keeps; the
// service owns the channel and the pools and sends what this package asks
// it to, so the query path runs in host tests against a real stack.
//
// Each query gets an endpoint of its own: Open, then Connect to the server's
// port 53, which binds a random ephemeral port, then Send and RecvFrom. The
// connected endpoint keeps only datagrams from that address and port, and
// the resolver checks the transaction ID and question of what arrives. The
// endpoint closes as soon as its transaction stops waiting for that query,
// so a late answer has no endpoint left to reach. The resolver's own timers
// decide timeouts and retransmissions; each retransmission opens a new
// endpoint with a new port.
package upstream

import (
	"goos/userspace/dns"
	"goos/userspace/udpapi"
)

// Port is the DNS server port every query goes to.
const Port = 53

// Sender sends one GUDP1 request and reports whether it went.
type Sender interface {
	Send(request *udpapi.Request, correlation uint64) bool
}

type phase uint8

const (
	idle phase = iota
	// opening waits for the endpoint of the query held in query.
	opening
	// active has an endpoint that sent the query or is sending it.
	active
)

// query is what one resolver transaction holds for its query in flight.
type query struct {
	phase    phase
	open     uint64
	endpoint uint32
	server   [4]byte
	length   int
	data     [dns.DatagramSize]byte
}

// Client is the resolver's side of one UDP session. A method that returns
// false met a protocol violation or could not send; the caller then drops
// the session and calls Unbind.
type Client struct {
	online      bool
	tx, rx      []byte
	txSlots     udpapi.Slots
	queries     [dns.MaxTransactions]query
	correlation uint64
	request     udpapi.Request
	scratch     [dns.DatagramSize]byte
}

// Bind starts using a session whose TX pool is writable and RX pool
// readable.
func (c *Client) Bind(tx, rx []byte) {
	c.online, c.tx, c.rx = true, tx, rx
	c.txSlots.Reset(udpapi.TXSlots)
	for index := range c.queries {
		c.queries[index].phase = idle
	}
}

// Unbind forgets the session and every query on it.
func (c *Client) Unbind() {
	c.online, c.tx, c.rx = false, nil, nil
	for index := range c.queries {
		c.queries[index].phase = idle
	}
}

// Live counts the queries holding or opening an endpoint.
func (c *Client) Live() int {
	count := 0
	for index := range c.queries {
		if c.queries[index].phase != idle {
			count++
		}
	}
	return count
}

func (c *Client) next() uint64 {
	c.correlation++
	if c.correlation == 0 {
		c.correlation++
	}
	return c.correlation
}

func (c *Client) send(out Sender, correlation uint64) bool {
	return out.Send(&c.request, correlation)
}

// SendQueries starts every query the resolver has ready. Without a session
// a query fails at once, so the resolver tries its next server or gives up.
func (c *Client) SendQueries(resolver *dns.Resolver, now uint64, out Sender) bool {
	for {
		outgoing, ready := resolver.NextQuery(c.scratch[:])
		if !ready {
			return true
		}
		if !c.online {
			resolver.SendFailed(outgoing.Transaction, now)
			continue
		}
		if !c.release(outgoing.Transaction, out) {
			return false
		}
		current := &c.queries[outgoing.Transaction]
		current.server, current.length = outgoing.Server, outgoing.Length
		copy(current.data[:], c.scratch[:outgoing.Length])
		current.phase, current.open = opening, c.next()
		c.request = udpapi.Request{Opcode: udpapi.OpOpen}
		if !c.send(out, current.open) {
			return false
		}
	}
}

// ReleaseFinished closes the endpoints of transactions that stopped waiting.
func (c *Client) ReleaseFinished(resolver *dns.Resolver, out Sender) bool {
	if !c.online {
		return true
	}
	for index := range c.queries {
		if c.queries[index].phase != idle && !resolver.Waiting(index) && !c.release(index, out) {
			return false
		}
	}
	return true
}

// Answer handles one decoded reply.
func (c *Client) Answer(resolver *dns.Resolver, reply *udpapi.Reply, correlation, now uint64, out Sender) bool {
	if !c.online {
		return true
	}
	if reply.Opcode == udpapi.OpOpen {
		return c.opened(resolver, reply, correlation, now, out)
	}
	transaction := c.owning(reply.Endpoint)
	switch reply.Opcode {
	case udpapi.OpSend:
		if !c.txSlots.Return(reply.Slot) {
			return false
		}
		if reply.Result != udpapi.ResultOK && transaction >= 0 {
			return c.fail(resolver, transaction, now, out)
		}
	case udpapi.OpConnect:
		if reply.Result != udpapi.ResultOK && transaction >= 0 {
			return c.fail(resolver, transaction, now, out)
		}
	case udpapi.OpRecvFrom:
		if reply.Result != udpapi.ResultOK {
			return true
		}
		offset := udpapi.Offset(reply.Slot)
		if transaction >= 0 && reply.Port == Port && reply.Address == c.queries[transaction].server {
			resolver.Receive(transaction, reply.Address, c.rx[offset:offset+uint64(reply.Length)], now)
		}
		c.request = udpapi.Request{Opcode: udpapi.OpRelease, Slot: reply.Slot}
		if !c.send(out, 0) {
			return false
		}
		// A reply the resolver did not take, such as a mismatched ID, leaves
		// the query waiting for the real one.
		if transaction >= 0 && resolver.Waiting(transaction) {
			c.request = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: reply.Endpoint}
			return c.send(out, c.next())
		}
	}
	return true
}

// opened takes a new endpoint and sends its query. One whose transaction
// has moved on is closed at once.
func (c *Client) opened(resolver *dns.Resolver, reply *udpapi.Reply, correlation, now uint64, out Sender) bool {
	transaction := -1
	for index := range c.queries {
		if c.queries[index].phase == opening && c.queries[index].open == correlation {
			transaction = index
		}
	}
	if transaction < 0 {
		if reply.Result == udpapi.ResultOK {
			return c.close(reply.Endpoint, out)
		}
		return true
	}
	current := &c.queries[transaction]
	if reply.Result != udpapi.ResultOK {
		current.phase = idle
		resolver.SendFailed(transaction, now)
		return true
	}
	current.phase, current.endpoint = active, reply.Endpoint
	slot, free := c.txSlots.Free()
	if !free {
		return c.fail(resolver, transaction, now, out)
	}
	copy(c.tx[udpapi.Offset(slot):], current.data[:current.length])
	c.txSlots.Lend(slot)
	c.request = udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: current.endpoint, Address: current.server, Port: Port}
	if !c.send(out, c.next()) {
		return false
	}
	c.request = udpapi.Request{Opcode: udpapi.OpSend, Endpoint: current.endpoint, Slot: slot, Length: uint16(current.length)}
	if !c.send(out, c.next()) {
		return false
	}
	c.request = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: current.endpoint}
	return c.send(out, c.next())
}

func (c *Client) owning(endpoint uint32) int {
	for index := range c.queries {
		if c.queries[index].phase == active && c.queries[index].endpoint == endpoint {
			return index
		}
	}
	return -1
}

// fail ends a transaction's query as unsendable and closes its endpoint.
func (c *Client) fail(resolver *dns.Resolver, transaction int, now uint64, out Sender) bool {
	sent := c.release(transaction, out)
	resolver.SendFailed(transaction, now)
	return sent
}

// release forgets a transaction's query, closing its endpoint if it has one.
// An endpoint still opening is closed when it arrives.
func (c *Client) release(transaction int, out Sender) bool {
	current := &c.queries[transaction]
	wasActive := current.phase == active
	current.phase = idle
	if wasActive {
		return c.close(current.endpoint, out)
	}
	return true
}

func (c *Client) close(endpoint uint32, out Sender) bool {
	c.request = udpapi.Request{Opcode: udpapi.OpClose, Endpoint: endpoint}
	return c.send(out, c.next())
}
