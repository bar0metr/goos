package tcpserve

import (
	"testing"

	"goos/userspace/tcpapi"
)

// FuzzSession4ClientPage writes arbitrary bytes over a connected client's
// control page, as a broken client could. Deliver must fail the session or
// carry on, and it must not panic.
func FuzzSession4ClientPage(f *testing.F) {
	f.Add([]byte{}, uint8(0))
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0}, uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, at uint8) {
		p := newPair4(t)
		p.connected(9200)
		page := p.b.tx[tcpapi.Layout4.Offset(tcpapi.TXSlots4):][:4096]
		copy(page[int(at)*16:], data)
		for range 3 {
			fault, _ := p.b.s.Deliver(p.stack, p.now, p.out)
			if fault != FaultNone {
				if fault != FaultProtocol {
					t.Fatalf("fault %q", fault)
				}
				return
			}
		}
	})
}
