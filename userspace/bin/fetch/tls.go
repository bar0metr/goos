//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/userspace/cryptosys"
	"goos/userspace/fetch"
	"goos/userspace/network"
	"goos/userspace/runtime/arena"
	"goos/userspace/tlsclient"
	"goos/userspace/utility"
)

var systemRoots *tlsclient.Roots

func (a adapter) ConnectTLS(address network.IPv4, port uint16, host []byte) (fetch.Connection, error) {
	if systemRoots == nil {
		before := arena.Current()
		roots, err := tlsclient.LoadSystemRoots(&bulk)
		if err != nil {
			return nil, err
		}
		systemRoots = roots
		if validation.Enabled {
			utility.PrintText("TLSROOT_RESOURCE roots=")
			utility.PrintUint(uint64(roots.Count()))
			utility.PrintText(" der_bytes=")
			utility.PrintUint(uint64(roots.DERBytes()))
			utility.PrintText(" allocated=")
			utility.PrintUint(uint64(arena.Current() - before))
			utility.EndLine()
		}
	}
	raw, err := a.Connect(address, port)
	if err != nil {
		return nil, &fetch.TCPConnectError{Err: err}
	}
	socket := raw.(*socket)
	before := arena.Current()
	client, err := tlsclient.Open(socket, tlsclient.Config{
		Host: string(host), Roots: systemRoots,
		Random: cryptosys.Reader{}, Now: cryptosys.Now,
		SetDeadline: socket.SetDeadline,
	})
	if validation.Enabled {
		utility.PrintText("TLS_HANDSHAKE_RESOURCE allocated=")
		utility.PrintUint(uint64(arena.Current() - before))
		utility.PrintText(" heap_high=")
		utility.PrintUint(arena.Touched())
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
