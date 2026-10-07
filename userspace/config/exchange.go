package config

import (
	"goos/kernel/ipc"
	"goos/kernel/kcall"
)

// Transport is one client's end of a configuration session: send a request,
// wait a bounded number of ticks for the next message, read the clock in
// ticks. Services implement it over their channel; tests over a queue.
type Transport interface {
	Send(message *ipc.Message) bool
	Receive(ticks uint64, out *ipc.Message) kcall.Status
	Now() uint64
}

// Outcome is how one exchange ended.
type Outcome uint8

const (
	OutcomeOK Outcome = iota
	// OutcomeTimeout means the deadline passed before the matching reply,
	// or had already passed before anything was sent.
	OutcomeTimeout
	// OutcomeFailed means the session broke or the service spoke out of
	// protocol: a peer that closed, a message that does not decode, a reply
	// to a request not yet sent, or a reply to another operation.
	OutcomeFailed
)

// Client numbers a session's requests. Correlations only grow and skip zero,
// so any reply with a smaller one answers a request that already timed out.
type Client struct {
	sequence uint64
}

// Next returns the correlation for the next request.
func (client *Client) Next() uint64 {
	client.sequence++
	if client.sequence == 0 {
		client.sequence++
	}
	return client.sequence
}

// Current returns the correlation of the request last numbered.
func (client *Client) Current() uint64 { return client.sequence }

// Exchange sends request, numbered with the client's next correlation, and
// waits until deadline, an absolute clock value in ticks, for the reply that
// carries it. Nothing is sent once the deadline has passed. A reply to an
// earlier request is a late answer to a request that timed out; Exchange drops
// it and keeps waiting without moving the deadline, so even a flood of them
// ends at the deadline. out holds the matching reply on OutcomeOK.
//
// Each wait asks for the whole ticks left before the deadline, so the last
// wait ends at the deadline and never one tick past it.
func (client *Client) Exchange(transport Transport, request *ipc.Message, deadline uint64, out *ipc.Message) Outcome {
	if transport.Now() >= deadline {
		return OutcomeTimeout
	}
	request.Correlation = client.Next()
	if !transport.Send(request) {
		return OutcomeFailed
	}
	for {
		now := transport.Now()
		if now >= deadline {
			return OutcomeTimeout
		}
		switch transport.Receive(deadline-now, out) {
		case kcall.StatusOK:
		case kcall.StatusTimeout:
			return OutcomeTimeout
		default:
			return OutcomeFailed
		}
		if out.Kind != ipc.KindReply || out.ProtocolID != request.ProtocolID || out.Correlation == 0 {
			return OutcomeFailed
		}
		switch {
		case out.Correlation < client.sequence:
			continue
		case out.Correlation > client.sequence:
			return OutcomeFailed
		}
		return OutcomeOK
	}
}

// ExchangeGet reads one key through Exchange and checks that the reply
// answers a Get. Any other operation is OutcomeFailed.
func (client *Client) ExchangeGet(transport Transport, key []byte, deadline uint64, wire *ipc.Message) (Value, Status, Outcome) {
	request, ok := EncodeRequest(Request{Operation: OperationGet, Key: key}, 1)
	if !ok {
		return Value{}, StatusInvalidArgument, OutcomeFailed
	}
	if outcome := client.Exchange(transport, &request, deadline, wire); outcome != OutcomeOK {
		return Value{}, StatusOK, outcome
	}
	reply, valid := DecodeReply(*wire)
	if !valid || reply.Operation != OperationGet {
		return Value{}, StatusOK, OutcomeFailed
	}
	return reply.Value, reply.Status, OutcomeOK
}
