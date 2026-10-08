// Package dnstcp is DNS-over-TCP framing (RFC 1035 section 4.2.2, RFC 7766):
// a two-byte big-endian length before each message. Sender holds one framed
// query and tracks how much of it the connection has accepted; Receiver
// assembles one response from stream pieces of any size. Neither knows about
// sockets, GTCP1 requests, transactions or output, so the resolver's fallback
// client and the dnsq command share them.
package dnstcp

import (
	"encoding/binary"

	"goos/userspace/dnswire"
)

const (
	// MaxMessage is the largest DNS message the length prefix can describe
	// and the size of every assembly buffer.
	MaxMessage = 65535
	PrefixSize = 2
)

// Buffer holds one assembled response.
type Buffer [MaxMessage]byte

// Sender is a framed query and its write progress: written bytes were
// accepted, and chunk went out in the last piece offered.
type Sender struct {
	framed  [PrefixSize + dnswire.MaxQuery]byte
	length  int
	written int
	chunk   int
}

// Set frames query, which must be at most dnswire.MaxQuery bytes.
func (s *Sender) Set(query []byte) {
	binary.BigEndian.PutUint16(s.framed[:PrefixSize], uint16(len(query)))
	s.length = PrefixSize + copy(s.framed[PrefixSize:], query)
	s.written, s.chunk = 0, 0
}

// Next returns the next piece to write, at most limit bytes, from the first
// byte the connection has not accepted.
func (s *Sender) Next(limit int) []byte {
	s.chunk = min(s.length-s.written, limit)
	return s.framed[s.written : s.written+s.chunk]
}

// Accepted records that the connection took count bytes of the last piece.
// It fails when count is more than that piece held.
func (s *Sender) Accepted(count int) bool {
	if count < 0 || count > s.chunk {
		return false
	}
	s.written += count
	s.chunk -= count
	return true
}

// Done reports that the whole framed query was accepted.
func (s *Sender) Done() bool { return s.written == s.length }

// Receiver assembles one length-prefixed message. received counts the stream
// bytes taken, prefix included; expected is the message length once both
// prefix bytes are in.
type Receiver struct {
	prefix   [PrefixSize]byte
	received int
	expected int
}

func (r *Receiver) Reset() { r.received, r.expected = 0, 0 }

// Want is how much to ask the stream for, at most limit: everything until
// the prefix is in, then no more than the rest of the message.
func (r *Receiver) Want(limit int) int {
	if r.received < PrefixSize {
		return limit
	}
	return min(r.expected-(r.received-PrefixSize), limit)
}

// Started reports that some of the response arrived.
func (r *Receiver) Started() bool { return r.received != 0 }

// Take adds stream bytes to buffer. It reports a complete message, and an
// invalid length prefix: shorter than a DNS header. Bytes after the message
// are ignored; a connection carries one.
func (r *Receiver) Take(buffer *Buffer, data []byte) (complete, valid bool) {
	for len(data) > 0 {
		if r.received < PrefixSize {
			r.prefix[r.received] = data[0]
			r.received++
			data = data[1:]
			if r.received == PrefixSize {
				r.expected = int(binary.BigEndian.Uint16(r.prefix[:]))
				if r.expected < dnswire.HeaderSize {
					return false, false
				}
			}
			continue
		}
		have := r.received - PrefixSize
		r.received += copy(buffer[have:r.expected], data)
		break
	}
	return r.Complete(), true
}

// Complete reports that the whole message is in.
func (r *Receiver) Complete() bool {
	return r.received >= PrefixSize && r.received-PrefixSize == r.expected
}

// Message is the assembled message once Complete.
func (r *Receiver) Message(buffer *Buffer) []byte { return buffer[:r.expected] }
