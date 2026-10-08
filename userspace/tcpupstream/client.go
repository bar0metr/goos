// Package tcpupstream carries the resolver's TCP fallback over a GTCP1
// session (ADR 0196). When a UDP answer has TC=1, the resolver core asks for
// the same query over TCP; this package opens a connection to the server's
// port 53, writes the query behind its two-byte length prefix, reads back
// exactly one length-prefixed message and hands it to the resolver. The
// service owns the channel and the pools and sends what this package asks
// it to, so the whole exchange runs in host tests against a real stack.
//
// Each fallback uses one connection for one query. Nothing here depends on
// how TCP or GTCP1 split the bytes: a Write may take part of what it was
// given and a Read may return any piece of the stream, the length prefix
// included. The resolver's timers decide timeouts; a fallback whose
// transaction stops waiting for it is closed at once.
package tcpupstream

import (
	"encoding/binary"

	"goos/userspace/dns"
	"goos/userspace/tcpapi"
)

const (
	// Port is the DNS server port every fallback connects to.
	Port = 53
	// MaxStreams bounds the fallbacks in flight. Each owns one assembly
	// buffer of dns.MaxStreamMessage bytes. A transaction that gets TC=1
	// while both are busy waits in the resolver for one to free, at most
	// until its own wait ends.
	MaxStreams = 2
	prefixSize = 2
)

// Sender sends one GTCP1 request and reports whether it went.
type Sender interface {
	Send(request *tcpapi.Request, correlation uint64) bool
}

type phase uint8

const (
	idle phase = iota
	opening
	connecting
	writing
	reading
)

// stream is one fallback: one resolver exchange on one connection. pending
// is the correlation of its one request in flight, so a reply for a stream
// that has been released, or reused, matches nothing.
type stream struct {
	phase       phase
	transaction int
	serial      uint32
	server      [4]byte
	endpoint    uint32
	pending     uint64
	// query holds the length prefix and the query, length bytes in all;
	// written of them were accepted and chunk went out in the last Write.
	query   [prefixSize + dns.MaxQuery]byte
	length  int
	written int
	chunk   int
	// received counts the stream bytes taken, prefix included; expected is
	// the message length from the prefix once both bytes are in.
	prefix   [prefixSize]byte
	received int
	expected int
}

// Client is the resolver's side of one TCP session. A method that returns
// false met a protocol violation or could not send; the caller then drops
// the session and calls Unbind.
type Client struct {
	online      bool
	tx, rx      []byte
	txSlots     tcpapi.Slots
	streams     [MaxStreams]stream
	buffers     [MaxStreams][dns.MaxStreamMessage]byte
	correlation uint64
	request     tcpapi.Request
	scratch     [dns.MaxQuery]byte
}

// Bind starts using a session whose TX pool is writable and RX pool
// readable.
func (c *Client) Bind(tx, rx []byte) {
	c.online, c.tx, c.rx = true, tx, rx
	c.txSlots.Reset(tcpapi.TXSlots)
	for index := range c.streams {
		c.streams[index].phase = idle
	}
}

// Unbind forgets the session. Every fallback on it fails, so the resolver
// moves on at once rather than wait for its timers.
func (c *Client) Unbind(resolver *dns.Resolver, now uint64) {
	c.online, c.tx, c.rx = false, nil, nil
	c.txSlots.Reset(tcpapi.TXSlots)
	for index := range c.streams {
		current := &c.streams[index]
		if current.phase != idle {
			current.phase = idle
			resolver.StreamFailed(current.transaction, current.serial, false, now)
		}
	}
}

// Online reports whether a session is bound.
func (c *Client) Online() bool { return c.online }

// Live counts the fallbacks holding or opening a connection.
func (c *Client) Live() int {
	count := 0
	for index := range c.streams {
		if c.streams[index].phase != idle {
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

// Start opens a connection for every fallback the resolver has ready, while
// a stream is free. Without a session a fallback fails at once.
func (c *Client) Start(resolver *dns.Resolver, now uint64, out Sender) bool {
	for {
		index := c.free()
		if index < 0 {
			return true
		}
		outgoing, ready := resolver.NextStream(c.scratch[:])
		if !ready {
			return true
		}
		if !c.online {
			resolver.StreamFailed(outgoing.Transaction, outgoing.Serial, false, now)
			continue
		}
		// A transaction has one exchange at a time: an older stream of the
		// same transaction is over.
		for other := range c.streams {
			if c.streams[other].phase != idle && c.streams[other].transaction == outgoing.Transaction && !c.release(other, out) {
				return false
			}
		}
		current := &c.streams[index]
		current.transaction, current.serial, current.server = outgoing.Transaction, outgoing.Serial, outgoing.Server
		binary.BigEndian.PutUint16(current.query[:prefixSize], uint16(outgoing.Length))
		copy(current.query[prefixSize:], c.scratch[:outgoing.Length])
		current.length, current.written, current.chunk = prefixSize+outgoing.Length, 0, 0
		current.received, current.expected = 0, 0
		current.phase, current.pending = opening, c.next()
		c.request = tcpapi.Request{Opcode: tcpapi.OpOpen}
		if !c.send(out, current.pending) {
			return false
		}
	}
}

func (c *Client) free() int {
	for index := range c.streams {
		if c.streams[index].phase == idle {
			return index
		}
	}
	return -1
}

// ReleaseFinished closes the connections of fallbacks the resolver stopped
// waiting for: answered elsewhere, timed out, cancelled or reset.
func (c *Client) ReleaseFinished(resolver *dns.Resolver, out Sender) bool {
	if !c.online {
		return true
	}
	for index := range c.streams {
		current := &c.streams[index]
		if current.phase != idle && !resolver.StreamWaiting(current.transaction, current.serial) && !c.release(index, out) {
			return false
		}
	}
	return true
}

// Answer handles one decoded reply. TX slots come back in every Write reply
// and every successful Read lends an RX slot, whoever the endpoint belongs
// to now; both are settled before the reply is matched to a stream.
func (c *Client) Answer(resolver *dns.Resolver, reply *tcpapi.Reply, correlation, now uint64, out Sender) bool {
	if !c.online {
		return true
	}
	if reply.Opcode == tcpapi.OpWrite && !c.txSlots.Return(reply.Slot) {
		return false
	}
	lent := reply.Opcode == tcpapi.OpRead && reply.Result == tcpapi.ResultOK
	index := c.owning(correlation)
	if index < 0 {
		if lent {
			return c.releaseSlot(reply.Slot, out)
		}
		// An endpoint that opened for a fallback already over is closed.
		if reply.Opcode == tcpapi.OpOpen && reply.Result == tcpapi.ResultOK {
			return c.close(reply.Endpoint, out)
		}
		return true
	}
	current := &c.streams[index]
	current.pending = 0
	if !c.expected(current, reply) {
		return false
	}
	if reply.Result != tcpapi.ResultOK {
		return c.fail(resolver, index, reply.Result == tcpapi.ResultTimeout, now, out)
	}
	switch reply.Opcode {
	case tcpapi.OpOpen:
		current.endpoint, current.phase = reply.Endpoint, connecting
		current.pending = c.next()
		c.request = tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: current.endpoint, Address: current.server, Port: Port}
		return c.send(out, current.pending)
	case tcpapi.OpConnect:
		current.phase = writing
		return c.write(resolver, index, now, out)
	case tcpapi.OpWrite:
		if int(reply.Length) > current.chunk {
			return false
		}
		current.written += int(reply.Length)
		if current.written < current.length {
			return c.write(resolver, index, now, out)
		}
		current.phase = reading
		return c.read(current, out)
	}
	return c.received(resolver, index, reply, now, out)
}

// expected checks that a reply answers the request the stream has out: the
// same operation on the stream's endpoint, and no more bytes than asked for.
func (c *Client) expected(current *stream, reply *tcpapi.Reply) bool {
	switch current.phase {
	case opening:
		return reply.Opcode == tcpapi.OpOpen
	case connecting:
		return reply.Opcode == tcpapi.OpConnect && reply.Endpoint == current.endpoint
	case writing:
		return reply.Opcode == tcpapi.OpWrite && reply.Endpoint == current.endpoint
	case reading:
		return reply.Opcode == tcpapi.OpRead && reply.Endpoint == current.endpoint && int(reply.Length) <= readSize(current)
	}
	return false
}

func (c *Client) owning(correlation uint64) int {
	for index := range c.streams {
		if c.streams[index].phase != idle && c.streams[index].pending == correlation {
			return index
		}
	}
	return -1
}

// write sends the next piece of the framed query, from the first byte the
// connection has not accepted yet.
func (c *Client) write(resolver *dns.Resolver, index int, now uint64, out Sender) bool {
	current := &c.streams[index]
	slot, free := c.txSlots.Free()
	if !free {
		return c.fail(resolver, index, false, now, out)
	}
	current.chunk = min(current.length-current.written, tcpapi.SlotSize)
	copy(c.tx[tcpapi.Offset(slot):], current.query[current.written:current.written+current.chunk])
	c.txSlots.Lend(slot)
	current.pending = c.next()
	c.request = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: current.endpoint, Slot: slot, Length: uint16(current.chunk)}
	return c.send(out, current.pending)
}

// readSize asks for a whole slot until the prefix is in, then for no more
// than the rest of the message.
func readSize(current *stream) int {
	if current.received < prefixSize {
		return tcpapi.SlotSize
	}
	return min(current.expected-(current.received-prefixSize), tcpapi.SlotSize)
}

func (c *Client) read(current *stream, out Sender) bool {
	current.pending = c.next()
	c.request = tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: current.endpoint, Length: uint16(readSize(current))}
	return c.send(out, current.pending)
}

// received copies one Read's bytes into the stream and gives the RX slot
// back. A whole message goes to the resolver and ends the fallback.
func (c *Client) received(resolver *dns.Resolver, index int, reply *tcpapi.Reply, now uint64, out Sender) bool {
	current := &c.streams[index]
	offset := tcpapi.Offset(reply.Slot)
	complete, valid := current.take(&c.buffers[index], c.rx[offset:offset+uint64(reply.Length)])
	if !c.releaseSlot(reply.Slot, out) {
		return false
	}
	switch {
	case !valid:
		return c.fail(resolver, index, false, now, out)
	case !complete:
		return c.read(current, out)
	}
	transaction, serial := current.transaction, current.serial
	if !c.release(index, out) {
		return false
	}
	resolver.ReceiveStream(transaction, serial, c.buffers[index][:current.expected], now)
	return true
}

// take adds stream bytes to buffer. It reports a complete message, and an
// invalid length prefix: shorter than a DNS header or longer than the
// buffer. Bytes after the message are ignored; the connection carries one.
func (current *stream) take(buffer *[dns.MaxStreamMessage]byte, data []byte) (bool, bool) {
	for len(data) > 0 {
		if current.received < prefixSize {
			current.prefix[current.received] = data[0]
			current.received++
			data = data[1:]
			if current.received == prefixSize {
				current.expected = int(binary.BigEndian.Uint16(current.prefix[:]))
				if current.expected < dns.MinMessage || current.expected > len(buffer) {
					return false, false
				}
			}
			continue
		}
		have := current.received - prefixSize
		count := copy(buffer[have:current.expected], data)
		current.received += count
		break
	}
	return current.received >= prefixSize && current.received-prefixSize == current.expected, true
}

// fail ends a fallback that got no whole answer and closes its connection.
func (c *Client) fail(resolver *dns.Resolver, index int, timedOut bool, now uint64, out Sender) bool {
	current := &c.streams[index]
	transaction, serial := current.transaction, current.serial
	sent := c.release(index, out)
	resolver.StreamFailed(transaction, serial, timedOut, now)
	return sent
}

// release forgets a stream, closing its endpoint if it has one. An endpoint
// still opening is closed when it arrives. Closing an endpoint ends its
// requests in flight, and their replies still return the slots they hold.
func (c *Client) release(index int, out Sender) bool {
	current := &c.streams[index]
	open := current.phase != idle && current.phase != opening
	current.phase, current.pending = idle, 0
	if open {
		return c.close(current.endpoint, out)
	}
	return true
}

func (c *Client) close(endpoint uint32, out Sender) bool {
	c.request = tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint}
	return c.send(out, c.next())
}

func (c *Client) releaseSlot(slot uint8, out Sender) bool {
	c.request = tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
	return c.send(out, 0)
}
