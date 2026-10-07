package config

import (
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"testing"
)

// fakeSession is a configuration service seen from the client: a queue of
// messages that arrive at given ticks, and a clock the waits advance.
type fakeSession struct {
	now      uint64
	sent     []ipc.Message
	queue    []arrival
	waits    []uint64
	failSend bool
	died     bool
}

type arrival struct {
	at      uint64
	message ipc.Message
	corrupt bool
}

func (session *fakeSession) Send(message *ipc.Message) bool {
	if session.failSend {
		return false
	}
	session.sent = append(session.sent, *message)
	return true
}

func (session *fakeSession) Now() uint64 { return session.now }

func (session *fakeSession) Receive(ticks uint64, out *ipc.Message) kcall.Status {
	session.waits = append(session.waits, ticks)
	if len(session.queue) == 0 || session.queue[0].at > session.now+ticks {
		session.now += ticks
		if session.died {
			return kcall.StatusPeerDied
		}
		return kcall.StatusTimeout
	}
	next := session.queue[0]
	session.queue = session.queue[1:]
	if next.at > session.now {
		session.now = next.at
	}
	if next.corrupt {
		return kcall.StatusCorruptData
	}
	*out = next.message
	return kcall.StatusOK
}

func getReply(t *testing.T, correlation uint64, value Value) ipc.Message {
	t.Helper()
	message, ok := EncodeReply(Reply{Operation: OperationGet, Status: StatusOK, Key: []byte("system.net.mode"), Value: value}, correlation)
	if !ok {
		t.Fatal("encode reply")
	}
	return message
}

func TestExchangeMatchingReply(t *testing.T) {
	session := &fakeSession{}
	var client Client
	session.queue = []arrival{{at: 3, message: getReply(t, 1, Uint(7))}}
	var wire ipc.Message
	value, status, outcome := client.ExchangeGet(session, []byte("system.net.mode"), 100, &wire)
	if outcome != OutcomeOK || status != StatusOK || value.Uint != 7 || session.sent[0].Correlation != 1 {
		t.Fatalf("outcome %d status %d value %+v", outcome, status, value)
	}
}

func TestExchangeDropsStaleReplies(t *testing.T) {
	for _, stale := range []int{1, 5} {
		session := &fakeSession{}
		var client Client
		client.sequence = 10
		for index := 0; index < stale; index++ {
			session.queue = append(session.queue, arrival{at: uint64(index), message: getReply(t, uint64(5+index%5), Uint(1))})
		}
		session.queue = append(session.queue, arrival{at: 20, message: getReply(t, 11, Uint(42))})
		var wire ipc.Message
		value, _, outcome := client.ExchangeGet(session, []byte("system.net.mode"), 100, &wire)
		if outcome != OutcomeOK || value.Uint != 42 {
			t.Fatalf("%d stale: outcome %d value %d", stale, outcome, value.Uint)
		}
	}
}

// The regression the reload path hit: a request times out, its reply turns up
// late, and the next request must still get its own reply.
func TestExchangeTimeoutThenLateReplyThenSuccess(t *testing.T) {
	session := &fakeSession{}
	var client Client
	var wire ipc.Message
	if _, _, outcome := client.ExchangeGet(session, []byte("system.net.mode"), 10, &wire); outcome != OutcomeTimeout {
		t.Fatalf("first exchange %d", outcome)
	}
	session.queue = []arrival{
		{at: session.now + 1, message: getReply(t, 1, Uint(1))},
		{at: session.now + 2, message: getReply(t, 2, Uint(2))},
	}
	value, _, outcome := client.ExchangeGet(session, []byte("system.net.mode"), session.now+50, &wire)
	if outcome != OutcomeOK || value.Uint != 2 {
		t.Fatalf("second exchange %d value %d", outcome, value.Uint)
	}
	if len(session.queue) != 0 {
		t.Fatal("reply left queued")
	}
}

func TestExchangeFailures(t *testing.T) {
	var wire ipc.Message
	future := &fakeSession{queue: []arrival{{at: 1, message: getReply(t, 9, Uint(1))}}}
	var client Client
	if _, _, outcome := client.ExchangeGet(future, []byte("a.b"), 100, &wire); outcome != OutcomeFailed {
		t.Fatalf("future correlation: %d", outcome)
	}
	set, _ := EncodeReply(Reply{Operation: OperationSet, Status: StatusOK}, 1)
	wrong := &fakeSession{queue: []arrival{{at: 1, message: set}}}
	client = Client{}
	if _, _, outcome := client.ExchangeGet(wrong, []byte("a.b"), 100, &wire); outcome != OutcomeFailed {
		t.Fatalf("wrong operation: %d", outcome)
	}
	malformed := getReply(t, 1, Uint(1))
	malformed.Payload[5] = 9
	bad := &fakeSession{queue: []arrival{{at: 1, message: malformed}}}
	client = Client{}
	if _, _, outcome := client.ExchangeGet(bad, []byte("a.b"), 100, &wire); outcome != OutcomeFailed {
		t.Fatalf("malformed reply: %d", outcome)
	}
	corrupt := &fakeSession{queue: []arrival{{at: 1, corrupt: true}}}
	client = Client{}
	if _, _, outcome := client.ExchangeGet(corrupt, []byte("a.b"), 100, &wire); outcome != OutcomeFailed {
		t.Fatalf("corrupt message: %d", outcome)
	}
	dead := &fakeSession{died: true}
	client = Client{}
	if _, _, outcome := client.ExchangeGet(dead, []byte("a.b"), 100, &wire); outcome != OutcomeFailed {
		t.Fatalf("peer died: %d", outcome)
	}
	refused := &fakeSession{failSend: true}
	client = Client{}
	if _, _, outcome := client.ExchangeGet(refused, []byte("a.b"), 100, &wire); outcome != OutcomeFailed {
		t.Fatalf("send failed: %d", outcome)
	}
}

func TestExchangeDeadlineIsStrict(t *testing.T) {
	var wire ipc.Message
	// Nothing is sent once the deadline has passed.
	late := &fakeSession{now: 50}
	var client Client
	if _, _, outcome := client.ExchangeGet(late, []byte("a.b"), 50, &wire); outcome != OutcomeTimeout || len(late.sent) != 0 {
		t.Fatalf("sent after the deadline: %d %d", outcome, len(late.sent))
	}
	// A flood of stale replies ends at the original deadline, and no wait
	// reaches past it.
	flood := &fakeSession{}
	client = Client{sequence: 100}
	for tick := uint64(0); tick < 200; tick++ {
		flood.queue = append(flood.queue, arrival{at: tick, message: getReply(t, 1+tick%50, Uint(1))})
	}
	if _, _, outcome := client.ExchangeGet(flood, []byte("a.b"), 30, &wire); outcome != OutcomeTimeout {
		t.Fatalf("flood: %d", outcome)
	}
	if flood.now > 30 {
		t.Fatalf("waited until tick %d past deadline 30", flood.now)
	}
	for _, wait := range flood.waits {
		if wait == 0 {
			t.Fatal("a zero-tick wait")
		}
	}
}
