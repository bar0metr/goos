//go:build goos_userspace

package fetchtransport

import (
	"goos/internal/validation"
	"goos/userspace/cryptosys"
	"goos/userspace/fetch"
	"goos/userspace/network"
	"goos/userspace/runtime/arena"
	"goos/userspace/tlsclient"
	"goos/userspace/utility"
)

func (a *Adapter) ConnectTLS(address network.IPv4, port uint16, host []byte) (fetch.Connection, error) {
	if a.roots == nil {
		before := arena.HeapStats().TotalAlloc
		roots, err := tlsclient.LoadSystemRoots(&a.bulk)
		if err != nil {
			return nil, err
		}
		a.roots = roots
		if validation.Enabled {
			utility.PrintText("TLSROOT_RESOURCE roots=")
			utility.PrintUint(uint64(roots.Count()))
			utility.PrintText(" der_bytes=")
			utility.PrintUint(uint64(roots.DERBytes()))
			utility.PrintText(" allocated=")
			utility.PrintUint(arena.HeapStats().TotalAlloc - before)
			utility.EndLine()
		}
	}
	raw, err := a.Connect(address, port)
	if err != nil {
		return nil, &fetch.TCPConnectError{Err: err}
	}
	socket := raw.(*socket)
	before := arena.HeapStats().TotalAlloc
	client, err := tlsclient.Open(socket, tlsclient.Config{
		Host: string(host), Roots: a.roots,
		Random: cryptosys.Reader{}, Now: cryptosys.Now,
		SetDeadline: socket.SetDeadline,
	})
	if validation.Enabled {
		utility.PrintText("TLS_HANDSHAKE_RESOURCE allocated=")
		utility.PrintUint(arena.HeapStats().TotalAlloc - before)
		utility.PrintText(" heap_high=")
		utility.PrintUint(arena.HeapStats().PeakHeld)
		if err == nil {
			utility.PrintText(" cipher=")
			utility.PrintUint(uint64(client.CipherSuite()))
		}
		utility.EndLine()
	}
	if err != nil {
		socket.Close()
		return nil, err
	}
	return client, nil
}
