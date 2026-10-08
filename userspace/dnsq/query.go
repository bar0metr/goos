package dnsq

import (
	"goos/userspace/dnstcp"
	"goos/userspace/dnswire"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
)

const (
	millisecond = uint64(1_000_000)
	// closeWait bounds the Close that ends every exchange, even one that
	// ran out of time.
	closeWait = 1000 * millisecond
)

// Net is the pair of sessions one query uses. Each call sends one request
// and waits for its reply until deadline, on the clock Now reads in
// nanoseconds; false means no reply came in time or the session failed.
// Payload is TX slot 0, where a request's data goes.
type Net interface {
	Now() uint64
	UDPCall(request *udpapi.Request, deadline uint64, reply *udpapi.Reply) bool
	UDPPayload() []byte
	UDPReceived(reply *udpapi.Reply) []byte
	UDPRelease(slot uint8)
	TCPCall(request *tcpapi.Request, deadline uint64, reply *tcpapi.Reply) bool
	TCPPayload() []byte
	TCPReceived(reply *tcpapi.Reply) []byte
	TCPRelease(slot uint8)
}

// Output is the terminal: text a piece at a time, and line ends.
type Output interface {
	dnswire.Writer
	Line()
}

// Failure names the step that failed and why. A zero Failure is success.
type Failure struct {
	Step   string
	Reason string
}

func (f Failure) Failed() bool { return f.Reason != "" }

// Query is one query's whole state, the response buffer included. It is
// large, so the command keeps it in a package variable.
type Query struct {
	options  *Options
	id       uint16
	start    uint64
	deadline uint64
	query    [dnswire.MaxQuery]byte
	length   int
	sender   dnstcp.Sender
	receiver dnstcp.Receiver
	// buffer holds the response from either transport: a datagram fits
	// one RX slot, a TCP message at most dnstcp.MaxMessage bytes.
	buffer     dnstcp.Buffer
	size       int
	tcp        bool
	udpRequest udpapi.Request
	udpReply   udpapi.Reply
	tcpRequest tcpapi.Request
	tcpReply   tcpapi.Reply
	response   dnswire.Response
	owner      dnswire.Name
	scratch    dnswire.Name
	record     dnswire.Record
}

// Run sends the query described by options with DNS ID id and prints the
// response. It returns 0 for any complete, valid response whatever its
// rcode, and 1 with the failure otherwise.
func (q *Query) Run(options *Options, id uint16, net Net, out Output) (int, Failure) {
	q.options, q.id = options, id
	q.start = net.Now()
	q.deadline = q.start + options.Timeout*millisecond
	q.length = dnswire.EncodeQuery(q.query[:], id, &options.Name, options.Type, options.EDNS)
	if options.Mode != TCPOnly {
		q.tcp = false
		if failure := q.exchangeUDP(net); failure.Failed() {
			return 1, failure
		}
		if failure := q.check("udp"); failure.Failed() {
			return 1, failure
		}
		if !q.response.Truncated {
			q.print(net, out)
			return 0, Failure{}
		}
		if options.Mode == UDPOnly {
			return 1, Failure{"udp", "response truncated (TC=1), TCP fallback off"}
		}
		if !options.Short {
			out.Text(";; UDP response truncated; retrying over TCP")
			out.Line()
		}
	}
	q.tcp = true
	if failure := q.exchangeTCP(net); failure.Failed() {
		return 1, failure
	}
	if failure := q.check("tcp"); failure.Failed() {
		return 1, failure
	}
	if q.response.Truncated {
		return 1, Failure{"tcp", "response truncated (TC=1)"}
	}
	q.print(net, out)
	return 0, Failure{}
}

// check validates the response against the query, and then the data of
// every record that will be shown, before anything of it is printed.
func (q *Query) check(step string) Failure {
	message := q.buffer[:q.size]
	switch dnswire.ParseResponse(message, len(message), q.id, &q.options.Name, q.options.Type, &q.scratch, &q.response) {
	case dnswire.ParseMismatched:
		return Failure{step, "response does not match the query"}
	case dnswire.ParseMalformed:
		return Failure{step, "malformed response"}
	}
	if q.response.Truncated {
		return Failure{}
	}
	position := q.response.AnswerAt
	for index := 0; index < q.response.Answers+q.response.Authorities+q.response.Additionals; index++ {
		next, _ := dnswire.ReadRecord(message, position, &q.owner, &q.record)
		if !dnswire.ValidRData(message, &q.record, &q.scratch) {
			return Failure{step, "malformed record data"}
		}
		position = next
	}
	return Failure{}
}

func (q *Query) callUDP(net Net, step string, deadline uint64) Failure {
	if !net.UDPCall(&q.udpRequest, deadline, &q.udpReply) {
		return Failure{step, "timed out"}
	}
	if q.udpReply.Result != udpapi.ResultOK {
		return Failure{step, q.udpReply.Result.Text()}
	}
	return Failure{}
}

// exchangeUDP sends the query in one datagram and takes one back. The
// endpoint is closed on every path, which also ends a RecvFrom still
// waiting.
func (q *Query) exchangeUDP(net Net) Failure {
	q.udpRequest = udpapi.Request{Opcode: udpapi.OpOpen}
	if failure := q.callUDP(net, "udp open", q.deadline); failure.Failed() {
		return failure
	}
	endpoint := q.udpReply.Endpoint
	failure := q.datagram(net, endpoint)
	q.udpRequest = udpapi.Request{Opcode: udpapi.OpClose, Endpoint: endpoint}
	net.UDPCall(&q.udpRequest, net.Now()+closeWait, &q.udpReply)
	return failure
}

func (q *Query) datagram(net Net, endpoint uint32) Failure {
	q.udpRequest = udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: endpoint, Address: q.options.Server, Port: Port}
	if failure := q.callUDP(net, "udp connect", q.deadline); failure.Failed() {
		return failure
	}
	copy(net.UDPPayload(), q.query[:q.length])
	q.udpRequest = udpapi.Request{Opcode: udpapi.OpSend, Endpoint: endpoint, Length: uint16(q.length)}
	if failure := q.callUDP(net, "udp send", q.deadline); failure.Failed() {
		return failure
	}
	q.udpRequest = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: endpoint}
	if failure := q.callUDP(net, "udp receive", q.deadline); failure.Failed() {
		return failure
	}
	q.size = copy(q.buffer[:], net.UDPReceived(&q.udpReply))
	net.UDPRelease(q.udpReply.Slot)
	return Failure{}
}

func (q *Query) callTCP(net Net, step string, deadline uint64) Failure {
	if !net.TCPCall(&q.tcpRequest, deadline, &q.tcpReply) {
		return Failure{step, "timed out"}
	}
	if q.tcpReply.Result != tcpapi.ResultOK {
		return Failure{step, q.tcpReply.Result.Text()}
	}
	return Failure{}
}

// exchangeTCP writes the framed query on one connection and reads one
// framed response. The connection is closed on every path; GTCP1 takes it
// through FIN and TIME_WAIT like any other.
func (q *Query) exchangeTCP(net Net) Failure {
	q.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpOpen}
	if failure := q.callTCP(net, "tcp open", q.deadline); failure.Failed() {
		return failure
	}
	endpoint := q.tcpReply.Endpoint
	failure := q.stream(net, endpoint)
	q.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint}
	net.TCPCall(&q.tcpRequest, net.Now()+closeWait, &q.tcpReply)
	return failure
}

func (q *Query) stream(net Net, endpoint uint32) Failure {
	q.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: endpoint, Address: q.options.Server, Port: Port}
	if failure := q.callTCP(net, "tcp connect", q.deadline); failure.Failed() {
		return failure
	}
	// A Write may take only part of what it was given; the rest goes again.
	q.sender.Set(q.query[:q.length])
	for !q.sender.Done() {
		length := copy(net.TCPPayload(), q.sender.Next(tcpapi.SlotSize))
		q.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: endpoint, Length: uint16(length)}
		if failure := q.callTCP(net, "tcp write", q.deadline); failure.Failed() {
			return failure
		}
		if !q.sender.Accepted(int(q.tcpReply.Length)) {
			return Failure{"tcp write", "service accepted more than was written"}
		}
	}
	// A Read returns any piece of the stream, the length prefix included.
	q.receiver.Reset()
	for {
		want := q.receiver.Want(tcpapi.SlotSize)
		q.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: endpoint, Length: uint16(want)}
		if !net.TCPCall(&q.tcpRequest, q.deadline, &q.tcpReply) {
			return Failure{"tcp read", "timed out"}
		}
		switch q.tcpReply.Result {
		case tcpapi.ResultOK:
		case tcpapi.ResultEOF:
			return Failure{"tcp read", "connection closed before the whole response"}
		default:
			return Failure{"tcp read", q.tcpReply.Result.Text()}
		}
		data := net.TCPReceived(&q.tcpReply)
		if len(data) > want {
			net.TCPRelease(q.tcpReply.Slot)
			return Failure{"tcp read", "service returned more than was asked"}
		}
		complete, valid := q.receiver.Take(&q.buffer, data)
		net.TCPRelease(q.tcpReply.Slot)
		switch {
		case !valid:
			return Failure{"tcp read", "length prefix shorter than a DNS header"}
		case complete:
			q.size = len(q.receiver.Message(&q.buffer))
			return Failure{}
		}
	}
}
