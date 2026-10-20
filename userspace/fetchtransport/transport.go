//go:build goos_userspace

// Package fetchtransport binds the native resolver, TCP, TLS and VFS adapters.
//
// An Adapter drives the command's one TCP session (utility.OpenTCP), which
// belongs to one goroutine at a time, so an Adapter and the connections and
// outputs it returns belong to one goroutine too. Its request and reply, its
// path buffer and its file buffer live in the Adapter, not in the package.
package fetchtransport

import (
	"errors"
	"goos/kernel/clock"
	"goos/kernel/vfs"
	"goos/userspace/fetch"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/resolvapi"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/tlsclient"
	"goos/userspace/utility"
	"io"
)

func New(data startup.Data) (*Adapter, error) {
	if !utility.OpenTCP(data) {
		return nil, errors.New("TCP session not granted")
	}
	return &Adapter{CWD: data.CWD()}, nil
}

type Adapter struct {
	CWD   []byte
	roots *tlsclient.Roots
	tx    tcpapi.Request
	rx    tcpapi.Reply
	path  [shell.MaxPath]byte
	bulk  utility.Bulk
}

func (*Adapter) Resolve(host []byte, addresses *[8]network.IPv4) (int, error) {
	name, valid := hostnameName(host)
	if !valid {
		return 0, errors.New("invalid host name")
	}
	request := resolvapi.Request{Opcode: resolvapi.OpResolve, Name: name}
	var reply resolvapi.Reply
	if !utility.CallResolver(&request, &reply) {
		return 0, errors.New("resolver unavailable")
	}
	if reply.Result != resolvapi.ResultOK {
		return 0, errors.New(reply.Result.Text())
	}
	for i := 0; i < int(reply.Count); i++ {
		addresses[i] = network.IPv4(reply.Addresses[i])
	}
	return int(reply.Count), nil
}

func hostnameName(host []byte) (hostname.Name, bool) { return hostname.Parse(host) }

func (a *Adapter) Connect(address network.IPv4, port uint16) (fetch.Connection, error) {
	a.tx = tcpapi.Request{Opcode: tcpapi.OpOpen}
	if err := a.call(); err != nil {
		return nil, err
	}
	endpoint := a.rx.Endpoint
	a.tx = tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: endpoint, Address: address, Port: port}
	if err := a.call(); err != nil {
		a.closeEndpoint(endpoint)
		return nil, err
	}
	return &socket{adapter: a, endpoint: endpoint}, nil
}
func (a *Adapter) call() error { return a.callWithin(35 * clock.TicksPerSecond) }
func (a *Adapter) callWithin(timeout uint64) error {
	if !utility.TCPCall(&a.tx, timeout, &a.rx) {
		return errors.New("TCP timeout")
	}
	if a.rx.Result != tcpapi.ResultOK {
		return errors.New(a.rx.Result.Text())
	}
	return nil
}
func (a *Adapter) closeEndpoint(endpoint uint32) {
	a.tx = tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint}
	utility.TCPCall(&a.tx, 35*clock.TicksPerSecond, &a.rx)
}

type socket struct {
	adapter  *Adapter
	endpoint uint32
	deadline uint64
	closed   bool
}

func (s *socket) Close() {
	if !s.closed {
		s.closed = true
		s.adapter.closeEndpoint(s.endpoint)
	}
}
func (s *socket) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	timeout, err := s.timeout()
	if err != nil {
		return 0, err
	}
	n := len(p)
	if n > tcpapi.SlotSize {
		n = tcpapi.SlotSize
	}
	a := s.adapter
	copy(utility.TCPPayload(), p[:n])
	a.tx = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: s.endpoint, Length: uint16(n)}
	if err := a.callWithin(timeout); err != nil {
		return 0, err
	}
	if a.rx.Length == 0 || int(a.rx.Length) > n {
		return 0, errors.New("short TCP write")
	}
	return int(a.rx.Length), nil
}
func (s *socket) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	timeout, err := s.timeout()
	if err != nil {
		return 0, err
	}
	n := len(p)
	if n > tcpapi.SlotSize {
		n = tcpapi.SlotSize
	}
	a := s.adapter
	a.tx = tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: s.endpoint, Length: uint16(n)}
	if !utility.TCPCall(&a.tx, timeout, &a.rx) {
		return 0, errors.New("TCP timeout")
	}
	if a.rx.Result == tcpapi.ResultEOF {
		return 0, io.EOF
	}
	if a.rx.Result != tcpapi.ResultOK {
		return 0, errors.New(a.rx.Result.Text())
	}
	data := utility.TCPReceived(&a.rx)
	count := copy(p, data)
	utility.TCPRelease(a.rx.Slot)
	return count, nil
}

type output struct {
	adapter *Adapter
	file    vfs.Handle
	owned   bool
}

func (output) Close() error { return nil }
func (o output) Write(p []byte) error {
	if !o.owned {
		utility.Write(p)
		return nil
	}
	bulk := &o.adapter.bulk
	for len(p) > 0 {
		n := len(p)
		if n > len(bulk.Memory) {
			n = len(bulk.Memory)
		}
		copy(bulk.Memory, p[:n])
		reply, ok := bulk.WriteFile(o.file, n)
		if !ok || reply.Status != vfs.StatusOK || int(reply.Size) != n {
			return errors.New("file write failed")
		}
		p = p[n:]
	}
	return nil
}
func (o fileOutput) Write(p []byte) error {
	return output{adapter: o.adapter, file: o.file, owned: true}.Write(p)
}
func (o fileOutput) Close() error {
	_, status := utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: o.file})
	if status != vfs.StatusOK {
		return errors.New("file close failed")
	}
	return nil
}

type fileOutput struct {
	adapter *Adapter
	file    vfs.Handle
}

func (a *Adapter) Stdout() fetch.Output { return output{adapter: a} }
func (a *Adapter) Open(name []byte) (fetch.Output, error) {
	path, ok := shell.ResolvePath(a.CWD, name, &a.path)
	if !ok {
		return nil, errors.New("invalid file path")
	}
	if len(a.bulk.Memory) == 0 {
		var ok bool
		a.bulk, ok = utility.NewBulk(tcpapi.SlotSize)
		if !ok {
			return nil, errors.New("file buffer unavailable")
		}
	}
	status := utility.CreateFile(path)
	if status != vfs.StatusOK && status != vfs.StatusAlreadyExists {
		return nil, errors.New("file create failed")
	}
	_, status = utility.Request(vfs.Request{Operation: vfs.OperationTruncate, Path: path, Offset: 0})
	if status != vfs.StatusOK {
		return nil, errors.New("file truncate failed")
	}
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	if status != vfs.StatusOK {
		return nil, errors.New("file open failed")
	}
	return fileOutput{adapter: a, file: reply.Handle}, nil
}
func (*Adapter) Diagnostic(p []byte) { utility.WriteError(p) }
