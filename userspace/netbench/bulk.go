//go:build goos_userspace

package netbench

import (
	"goos/userspace/tcpapi"
	"goos/userspace/utility"
)

// streamBulk writes total bytes from client to server on a GTCP 3 session
// the way linuxsockd drives one: up to PipelinedWrites3 NoWait writes in
// flight, each naming the stream offset of its first byte, and up to
// QueuedReads3 reads parked on server. A write that takes a prefix makes
// the writes after it take nothing; once they all answered, the stream
// goes on from the first byte netd did not take, after the endpoint shows
// room again. Reads stay posted only while they can still be answered.
func streamBulk(client, server uint32, total uint64) {
	type write struct {
		correlation uint64
		slot        uint8
		length      uint32
	}
	var (
		writes   [tcpapi.PipelinedWrites3]write
		busy     [tcpapi.TXSlots3]bool
		reads    [tcpapi.QueuedReads3]uint64
		nwrites  int
		nreads   int
		accepted uint64 // bytes netd took
		ahead    uint64 // bytes in writes not answered yet
		received uint64
		stalled  bool
		ready    uint64 // a Ready or WaitReady on client is out
	)
	size := uint64(utility.TCPSlotSize())
	base := streamOffset(client)
	start := utility.ClockNanos()
	for received < total || accepted < total || nwrites != 0 {
		// Keep reads parked while the bytes left could answer them all.
		for nreads < len(reads) && uint64(nreads)*size < total-received {
			reads[nreads] = post(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: uint16(size)})
			nreads++
		}
		for !stalled && nwrites < len(writes) && accepted+ahead < total {
			slot := uint8(0)
			for busy[slot] {
				slot++
			}
			length := min(total-accepted-ahead, size)
			correlation := post(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Slot: slot, Length: uint16(length),
				Flags: tcpapi.FlagNoWait, Offset: *base + uint32(accepted+ahead)})
			busy[slot] = true
			writes[nwrites] = write{correlation: correlation, slot: slot, length: uint32(length)}
			nwrites++
			ahead += length
		}
		if stalled && nwrites == 0 && ready == 0 {
			ready = post(tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: client})
		}
		var reply tcpapi.Reply
		answered, ok := utility.TCPWait(wait, &reply)
		if !ok {
			utility.Fail([]byte("bulk stream"), "no reply")
		}
		switch {
		case answered == ready:
			ready = 0
			check("bulk ready", reply.Result)
			if reply.Mask&tcpapi.ReadyWritable != 0 {
				stalled = false
			} else {
				ready = post(tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: client, Mask: reply.Mask})
			}
		case reply.Opcode == tcpapi.OpWrite:
			index := 0
			for index < nwrites && writes[index].correlation != answered {
				index++
			}
			if index == nwrites {
				utility.Fail([]byte("bulk stream"), "unexpected write reply")
			}
			w := writes[index]
			writes[index] = writes[nwrites-1]
			nwrites--
			busy[w.slot] = false
			ahead -= uint64(w.length)
			taken := uint32(0)
			switch reply.Result {
			case tcpapi.ResultOK:
				taken = uint32(reply.Length)
			case tcpapi.ResultWouldBlock:
			default:
				check("bulk write", reply.Result)
			}
			if taken > w.length {
				utility.Fail([]byte("bulk stream"), "write took more than it carried")
			}
			accepted += uint64(taken)
			if taken < w.length {
				// The writes after this one take nothing; the stream resumes
				// at accepted once they answered and netd has room.
				stalled = true
			}
		case reply.Opcode == tcpapi.OpRead:
			index := 0
			for index < nreads && reads[index] != answered {
				index++
			}
			if index == nreads {
				utility.Fail([]byte("bulk stream"), "unexpected read reply")
			}
			reads[index] = reads[nreads-1]
			nreads--
			check("bulk read", reply.Result)
			received += uint64(copy(sink[:], utility.TCPReceived(&reply)))
			utility.TCPRelease(reply.Slot)
		default:
			utility.Fail([]byte("bulk stream"), "unexpected reply")
		}
	}
	*base += uint32(total)
	if received != total || accepted != total || nreads != 0 {
		utility.Fail([]byte("bulk stream"), "byte count")
	}
	elapsed := (utility.ClockNanos() - start) / 1000
	out := begin(prefix + "tcp-stream")
	out = field(out, "bytes", total)
	out = field(out, "us", elapsed)
	if elapsed != 0 {
		out = field(out, "MiBps", total*1000000/elapsed>>20)
	}
	finish(out)
}
