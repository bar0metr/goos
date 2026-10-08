package dnsq

import "goos/userspace/dnswire"

// separator sits between the fields of a record line.
const separator = "    "

var flagNames = [...]struct {
	bit   uint16
	lower string
	upper string
}{
	{dnswire.FlagQR, "qr", "QR"}, {dnswire.FlagAA, "aa", "AA"}, {dnswire.FlagTC, "tc", "TC"},
	{dnswire.FlagRD, "rd", "RD"}, {dnswire.FlagRA, "ra", "RA"}, {dnswire.FlagAD, "ad", "AD"},
	{dnswire.FlagCD, "cd", "CD"},
}

func (q *Query) print(net Net, out Output) {
	message := q.buffer[:q.size]
	if q.options.Short {
		q.records(out, message, q.response.AnswerAt, q.response.Answers, true)
		return
	}
	elapsed := (net.Now() - q.start) / millisecond
	out.Text(";; server: ")
	for index, octet := range q.options.Server {
		if index != 0 {
			out.Byte('.')
		}
		dnswire.FormatDecimal(out, uint64(octet))
	}
	out.Text(":53")
	out.Line()
	out.Text(";; transport: ")
	if q.tcp {
		out.Text("TCP")
	} else {
		out.Text("UDP")
	}
	out.Line()
	if q.options.Raw {
		q.printRaw(out, message)
	} else {
		q.printDefault(out, message)
	}
	out.Line()
	out.Text(";; ")
	dnswire.FormatDecimal(out, uint64(q.response.Answers))
	if q.response.Answers == 1 {
		out.Text(" answer, ")
	} else {
		out.Text(" answers, ")
	}
	dnswire.FormatDecimal(out, uint64(q.size))
	out.Text(" bytes, ")
	dnswire.FormatDecimal(out, elapsed)
	out.Text(" ms")
	out.Line()
}

func (q *Query) printDefault(out Output, message []byte) {
	out.Text(";; id: ")
	dnswire.FormatDecimal(out, uint64(q.id))
	out.Line()
	out.Text(";; status: ")
	dnswire.FormatRcode(out, q.response.Rcode)
	out.Line()
	q.printFlags(out, false)
	out.Line()
	out.Text(";; question:")
	out.Line()
	q.question(out)
	if q.response.Answers != 0 {
		out.Line()
		out.Text(";; answer:")
		out.Line()
		q.records(out, message, q.response.AnswerAt, q.response.Answers, false)
	}
}

func (q *Query) printRaw(out Output, message []byte) {
	out.Text(";; id: 0x")
	for shift := 12; shift >= 0; shift -= 4 {
		out.Byte("0123456789abcdef"[q.id>>shift&0x0f])
	}
	out.Line()
	out.Text(";; opcode: QUERY")
	out.Line()
	out.Text(";; rcode: ")
	dnswire.FormatRcode(out, q.response.Rcode)
	out.Line()
	q.printFlags(out, true)
	count(out, "questions", q.response.Questions)
	count(out, "answers", q.response.Answers)
	count(out, "authority", q.response.Authorities)
	count(out, "additional", q.response.Additionals)
	out.Text(";; edns: ")
	if q.response.HasOPT {
		out.Text("version ")
		dnswire.FormatDecimal(out, uint64(q.response.EDNSVersion))
		out.Text(" udp ")
		dnswire.FormatDecimal(out, uint64(q.response.EDNSPayload))
		if q.response.EDNSDO {
			out.Text(" do")
		}
	} else {
		out.Text("none")
	}
	out.Line()
	out.Line()
	out.Text("QUESTION")
	out.Line()
	q.question(out)
	q.section(out, message, "ANSWER", q.response.AnswerAt, q.response.Answers)
	q.section(out, message, "AUTHORITY", q.response.AuthorityAt, q.response.Authorities)
	q.section(out, message, "ADDITIONAL", q.response.AdditionalAt, q.response.Additionals)
}

func count(out Output, name string, value int) {
	out.Text(";; ")
	out.Text(name)
	out.Text(": ")
	dnswire.FormatDecimal(out, uint64(value))
	out.Line()
}

func (q *Query) printFlags(out Output, upper bool) {
	out.Text(";; flags:")
	for index := range flagNames {
		if q.response.Flags&flagNames[index].bit == 0 {
			continue
		}
		out.Byte(' ')
		if upper {
			out.Text(flagNames[index].upper)
		} else {
			out.Text(flagNames[index].lower)
		}
	}
	out.Line()
}

// question prints the question the response matched, which is the one
// asked.
func (q *Query) question(out Output) {
	dnswire.FormatName(out, &q.options.Name)
	out.Text(separator)
	out.Text("IN")
	out.Text(separator)
	dnswire.FormatType(out, q.options.Type)
	out.Line()
}

func (q *Query) section(out Output, message []byte, title string, at, records int) {
	out.Line()
	out.Text(title)
	out.Line()
	q.records(out, message, at, records, false)
}

// records prints count records from offset at, one per line, or only their
// data when short. The OPT pseudo-record is not data; the edns line shows
// it.
func (q *Query) records(out Output, message []byte, at, records int, short bool) {
	position := at
	for index := 0; index < records; index++ {
		next, _ := dnswire.ReadRecord(message, position, &q.owner, &q.record)
		position = next
		if q.record.Type == dnswire.TypeOPT {
			continue
		}
		if !short {
			dnswire.FormatName(out, &q.owner)
			out.Text(separator)
			dnswire.FormatDecimal(out, uint64(q.record.TTL))
			out.Text(separator)
			dnswire.FormatClass(out, q.record.Class)
			out.Text(separator)
			dnswire.FormatType(out, q.record.Type)
			out.Text(separator)
		}
		dnswire.FormatRData(out, message, &q.record, &q.scratch)
		out.Line()
	}
}
