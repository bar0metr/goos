package dnsq

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"goos/userspace/dnswire"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
)

// fake plays netd and a DNS server. It answers each request synchronously,
// checks that the client releases every RX slot it was lent and closes
// every endpoint it opened, and serves the response its answer function
// builds from the query it received.
type fake struct {
	t   *testing.T
	now uint64
	// step is how far the clock moves per request.
	step uint64

	udpAnswer func(query []byte) []byte
	tcpAnswer func(query []byte) []byte
	// udpResult and tcpResult fail the named operation.
	udpResult  map[udpapi.Opcode]udpapi.Result
	tcpResult  map[tcpapi.Opcode]tcpapi.Result
	udpSilent  bool // RecvFrom never answers
	tcpSilent  bool // Read never answers
	writeLimit int  // most bytes one Write takes; 0 for all
	readSizes  []int
	// tcpCut ends the stream after this many bytes when nonzero.
	tcpCut int

	udpTX, udpRX [udpapi.SlotSize]byte
	tcpTX, tcpRX [tcpapi.SlotSize]byte

	udpOpen, tcpOpen     int
	udpLent, tcpLent     int
	udpQueries           [][]byte
	tcpWritten           []byte
	tcpStream            []byte
	tcpConnects, writes  int
	readIndex, readCount int
}

func newFake(t *testing.T) *fake {
	return &fake{t: t, now: 1_000_000_000, step: millisecond, udpResult: map[udpapi.Opcode]udpapi.Result{},
		tcpResult: map[tcpapi.Opcode]tcpapi.Result{}}
}

func (f *fake) Now() uint64 { return f.now }

func (f *fake) UDPCall(request *udpapi.Request, deadline uint64, reply *udpapi.Reply) bool {
	f.now += f.step
	*reply = udpapi.Reply{Opcode: request.Opcode, Endpoint: request.Endpoint}
	if result, ok := f.udpResult[request.Opcode]; ok {
		reply.Result = result
		return true
	}
	switch request.Opcode {
	case udpapi.OpOpen:
		f.udpOpen++
		reply.Endpoint = 7
	case udpapi.OpConnect:
		if request.Port != Port || request.Endpoint != 7 {
			f.t.Fatalf("connect %+v", request)
		}
	case udpapi.OpSend:
		f.udpQueries = append(f.udpQueries, append([]byte(nil), f.udpTX[:request.Length]...))
	case udpapi.OpRecvFrom:
		if f.udpSilent {
			f.now = deadline
			return false
		}
		response := f.udpAnswer(f.udpQueries[len(f.udpQueries)-1])
		reply.Slot, reply.Length = 3, uint16(copy(f.udpRX[:], response))
		f.udpLent++
	case udpapi.OpClose:
		f.udpOpen--
	default:
		f.t.Fatalf("UDP opcode %d", request.Opcode)
	}
	return true
}

func (f *fake) UDPPayload() []byte                     { return f.udpTX[:] }
func (f *fake) UDPReceived(reply *udpapi.Reply) []byte { return f.udpRX[:reply.Length] }
func (f *fake) UDPRelease(slot uint8) {
	if slot != 3 || f.udpLent == 0 {
		f.t.Fatalf("UDP release %d", slot)
	}
	f.udpLent--
}

func (f *fake) TCPCall(request *tcpapi.Request, deadline uint64, reply *tcpapi.Reply) bool {
	f.now += f.step
	*reply = tcpapi.Reply{Opcode: request.Opcode, Endpoint: request.Endpoint}
	if result, ok := f.tcpResult[request.Opcode]; ok {
		reply.Result = result
		return true
	}
	switch request.Opcode {
	case tcpapi.OpOpen:
		f.tcpOpen++
		reply.Endpoint = 9
	case tcpapi.OpConnect:
		if request.Port != Port || request.Endpoint != 9 {
			f.t.Fatalf("connect %+v", request)
		}
		f.tcpConnects++
	case tcpapi.OpWrite:
		f.writes++
		take := int(request.Length)
		if f.writeLimit != 0 {
			take = min(take, f.writeLimit)
		}
		f.tcpWritten = append(f.tcpWritten, f.tcpTX[:take]...)
		reply.Length = uint16(take)
		if len(f.tcpWritten) >= 2 && len(f.tcpWritten) == 2+int(binary.BigEndian.Uint16(f.tcpWritten)) {
			f.tcpStream = f.tcpAnswer(f.tcpWritten[2:])
			if f.tcpCut != 0 {
				f.tcpStream = f.tcpStream[:f.tcpCut]
			}
		}
	case tcpapi.OpRead:
		if f.tcpSilent {
			f.now = deadline
			return false
		}
		if request.Length == 0 || request.Length > tcpapi.SlotSize {
			f.t.Fatalf("read length %d", request.Length)
		}
		if len(f.tcpStream) == 0 {
			reply.Result = tcpapi.ResultEOF
			return true
		}
		size := int(request.Length)
		if len(f.readSizes) != 0 {
			size = min(size, f.readSizes[f.readIndex%len(f.readSizes)])
			f.readIndex++
		}
		size = min(size, len(f.tcpStream))
		copy(f.tcpRX[:], f.tcpStream[:size])
		f.tcpStream = f.tcpStream[size:]
		reply.Slot, reply.Length = 5, uint16(size)
		f.tcpLent++
		f.readCount++
	case tcpapi.OpClose:
		f.tcpOpen--
	default:
		f.t.Fatalf("TCP opcode %d", request.Opcode)
	}
	return true
}

func (f *fake) TCPPayload() []byte                     { return f.tcpTX[:] }
func (f *fake) TCPReceived(reply *tcpapi.Reply) []byte { return f.tcpRX[:reply.Length] }
func (f *fake) TCPRelease(slot uint8) {
	if slot != 5 || f.tcpLent == 0 {
		f.t.Fatalf("TCP release %d", slot)
	}
	f.tcpLent--
}

// settled checks that nothing is left open or lent.
func (f *fake) settled() {
	f.t.Helper()
	if f.udpOpen != 0 || f.tcpOpen != 0 || f.udpLent != 0 || f.tcpLent != 0 {
		f.t.Fatalf("open udp %d tcp %d, lent udp %d tcp %d", f.udpOpen, f.tcpOpen, f.udpLent, f.tcpLent)
	}
}

// screen collects output lines.
type screen struct{ strings.Builder }

func (s *screen) Byte(character byte) { s.WriteByte(character) }
func (s *screen) Text(text string)    { s.WriteString(text) }
func (s *screen) Line()               { s.WriteByte('\n') }

// args is a command line after the command name.
type args []string

func (a args) ArgCount() int { return len(a) }
func (a args) Arg(index int) ([]byte, bool) {
	if index == 0 {
		return []byte("dnsq"), true
	}
	if index < 1 || index > len(a) {
		return nil, false
	}
	return []byte(a[index-1]), true
}

// rr is one answer record for the test server. A zero class means IN.
type rr struct {
	name string
	kind uint16
	ttl  uint32
	data []byte
}

func wire(name string) []byte {
	var out []byte
	name = strings.TrimSuffix(name, ".")
	if name != "" {
		for _, label := range strings.Split(name, ".") {
			out = append(out, byte(len(label)))
			out = append(out, label...)
		}
	}
	return append(out, 0)
}

// reply builds a response to query: its ID and question, then flags and
// records. Records go in the answer section unless authority is set.
type reply struct {
	flags      uint16
	answers    []rr
	authority  []rr
	opt        bool
	idDelta    uint16
	otherName  string
	noQuestion bool
}

func (r reply) to(query []byte) []byte {
	out := append([]byte(nil), query[:dnswire.HeaderSize]...)
	binary.BigEndian.PutUint16(out[0:], binary.BigEndian.Uint16(query)+r.idDelta)
	binary.BigEndian.PutUint16(out[2:], dnswire.FlagQR|dnswire.FlagRD|dnswire.FlagRA|r.flags)
	question := query[dnswire.HeaderSize:]
	end := bytes.IndexByte(question, 0) + 1 + dnswire.QuestionFixed
	question = question[:end]
	if r.otherName != "" {
		question = append(wire(r.otherName), question[end-dnswire.QuestionFixed:]...)
	}
	if r.noQuestion {
		question = nil
		binary.BigEndian.PutUint16(out[4:], 0)
	}
	out = append(out, question...)
	binary.BigEndian.PutUint16(out[6:], uint16(len(r.answers)))
	binary.BigEndian.PutUint16(out[8:], uint16(len(r.authority)))
	binary.BigEndian.PutUint16(out[10:], 0)
	for _, section := range [][]rr{r.answers, r.authority} {
		for _, record := range section {
			out = append(out, wire(record.name)...)
			out = binary.BigEndian.AppendUint16(out, record.kind)
			out = binary.BigEndian.AppendUint16(out, dnswire.ClassIN)
			out = binary.BigEndian.AppendUint32(out, record.ttl)
			out = binary.BigEndian.AppendUint16(out, uint16(len(record.data)))
			out = append(out, record.data...)
		}
	}
	if r.opt {
		binary.BigEndian.PutUint16(out[10:], 1)
		out = append(out, 0, 0, dnswire.TypeOPT, 0x04, 0xd0, 0, 0, 0, 0, 0, 0)
	}
	return out
}

func (r reply) answer(query []byte) []byte { return r.to(query) }

func framed(message []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(message))), message...)
}
