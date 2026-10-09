//go:build goos_userspace

// Command fetch transfers one HTTP/1.1 response through resolver, TCP and VFS grants.
package main

import (
	"errors"
	"goos/kernel/clock"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/fetch"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/resolvapi"
	"goos/userspace/runtime/arena"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/utility"
	"io"
)

var manifest = commands.Manifest{
	Usage: "fetch [OPTIONS] URL", Summary: "transfer an HTTP response",
	Options: "-I -i -v -H HEADER -d DATA -o FILE -O -L",
	MinArgs: 1, MaxArgs: 99,
	Grants: commands.GrantResolve | commands.GrantTCP | commands.GrantVFS,
}

var options fetch.Options
var arguments [commands.MaxArgs][]byte
var tx tcpapi.Request
var rx tcpapi.Reply
var pathBuffer [shell.MaxPath]byte
var bulk utility.Bulk

//export userspaceMain
func userspaceMain() {
	arena.InitPackages()
	utility.Run(manifest, run)
}
func main() {}

func run(data startup.Data) {
	for i := 0; i < data.ArgCount(); i++ {
		arguments[i], _ = data.Arg(i + 1)
	}
	if fetch.Parse(arguments[:data.ArgCount()], &options) != nil {
		utility.Usage()
	}
	if !utility.OpenTCP(data) {
		utility.FailStatus(1, nil, "TCP session not granted")
	}
	if options.Output != nil || options.RemoteName {
		var ok bool
		bulk, ok = utility.NewBulk(tcpapi.SlotSize)
		if !ok {
			utility.FailStatus(1, nil, "file buffer unavailable")
		}
	}
	if err := fetch.Run(&options, adapter{cwd: data.CWD()}); err != nil {
		utility.FailStatus(1, nil, err.Error())
	}
}

type adapter struct{ cwd []byte }

func (adapter) Resolve(host []byte, addresses *[8]network.IPv4) (int, error) {
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

func (adapter) Connect(address network.IPv4, port uint16) (fetch.Connection, error) {
	tx = tcpapi.Request{Opcode: tcpapi.OpOpen}
	if err := call(); err != nil {
		return nil, err
	}
	endpoint := rx.Endpoint
	tx = tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: endpoint, Address: address, Port: port}
	if err := call(); err != nil {
		closeEndpoint(endpoint)
		return nil, err
	}
	return &socket{endpoint: endpoint}, nil
}
func call() error { return callWithin(35 * clock.TicksPerSecond) }
func callWithin(timeout uint64) error {
	if !utility.TCPCall(&tx, timeout, &rx) {
		return errors.New("TCP timeout")
	}
	if rx.Result != tcpapi.ResultOK {
		return errors.New(rx.Result.Text())
	}
	return nil
}
func closeEndpoint(endpoint uint32) {
	tx = tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint}
	utility.TCPCall(&tx, 35*clock.TicksPerSecond, &rx)
}

type socket struct {
	endpoint uint32
	deadline uint64
	closed   bool
}

func (s *socket) Close() {
	if !s.closed {
		s.closed = true
		closeEndpoint(s.endpoint)
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
	copy(utility.TCPPayload(), p[:n])
	tx = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: s.endpoint, Length: uint16(n)}
	if err := callWithin(timeout); err != nil {
		return 0, err
	}
	if rx.Length == 0 || int(rx.Length) > n {
		return 0, errors.New("short TCP write")
	}
	return int(rx.Length), nil
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
	tx = tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: s.endpoint, Length: uint16(n)}
	if !utility.TCPCall(&tx, timeout, &rx) {
		return 0, errors.New("TCP timeout")
	}
	if rx.Result == tcpapi.ResultEOF {
		return 0, io.EOF
	}
	if rx.Result != tcpapi.ResultOK {
		return 0, errors.New(rx.Result.Text())
	}
	data := utility.TCPReceived(&rx)
	count := copy(p, data)
	utility.TCPRelease(rx.Slot)
	return count, nil
}

type output struct {
	file  vfs.Handle
	owned bool
}

func (output) Close() error { return nil }
func (o output) Write(p []byte) error {
	if !o.owned {
		utility.Write(p)
		return nil
	}
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
func (o fileOutput) Write(p []byte) error { return output{file: o.file, owned: true}.Write(p) }
func (o fileOutput) Close() error {
	_, status := utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: o.file})
	if status != vfs.StatusOK {
		return errors.New("file close failed")
	}
	return nil
}

type fileOutput struct{ file vfs.Handle }

func (adapter) Stdout() fetch.Output { return output{} }
func (a adapter) Open(name []byte) (fetch.Output, error) {
	path, ok := shell.ResolvePath(a.cwd, name, &pathBuffer)
	if !ok {
		return nil, errors.New("invalid file path")
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
	return fileOutput{file: reply.Handle}, nil
}
func (adapter) Diagnostic(p []byte) { utility.WriteError(p) }
