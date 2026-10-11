//go:build goos_userspace

// Command dnsq sends one DNS query straight to a named server over GUDP1 or
// GTCP1 and prints what came back. It never asks the resolver service: no
// cache, hosts table or server list stands between the query and the
// server, so it shows whether UDP, TCP and the fallback between them work.
// The logic lives in userspace/dnsq.
package main

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/dnsq"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "dnsq @SERVER [OPTIONS] NAME [TYPE]",
	Summary: "direct DNS query",
	Options: "-t/--tcp -u/--udp --no-edns --timeout MS --short --raw",
	MinArgs: 2,
	MaxArgs: commands.Unbounded,
	Grants:  commands.GrantUDP | commands.GrantTCP,
}

var (
	options dnsq.Options
	query   dnsq.Query
	one     [1]byte
)

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	if problem, culprit := dnsq.Parse(data, &options); problem != "" {
		utility.Fail(culprit, problem)
	}
	if options.Mode != dnsq.TCPOnly && !utility.OpenUDP(data) {
		utility.FailStatus(1, nil, "UDP session not granted")
	}
	if options.Mode != dnsq.UDPOnly && !utility.OpenTCP(data) {
		utility.FailStatus(1, nil, "TCP session not granted")
	}
	random, status := sys.RandomUint64()
	if status != kcall.StatusOK {
		utility.FailStatus(1, nil, "random source unavailable")
	}
	code, failure := query.Run(&options, uint16(random), network{}, terminal{})
	if code != 0 {
		utility.FailStatus(uint64(code), []byte(failure.Step), failure.Reason)
	}
}

// network carries the query over the command's own sessions.
type network struct{}

func (network) Now() uint64 { return utility.ClockNanos() }

// ticks converts an absolute deadline to the wait left, rounded up. It is
// zero once the deadline has passed.
func ticks(deadline uint64) uint64 {
	now := utility.ClockNanos()
	if now >= deadline {
		return 0
	}
	return ((deadline-now)*clock.TicksPerSecond + 999_999_999) / 1_000_000_000
}

func (network) UDPCall(request *udpapi.Request, deadline uint64, reply *udpapi.Reply) bool {
	wait := ticks(deadline)
	return wait != 0 && utility.UDPCall(request, wait, reply)
}

func (network) UDPPayload() []byte                     { return utility.UDPPayload() }
func (network) UDPReceived(reply *udpapi.Reply) []byte { return utility.UDPReceived(reply) }
func (network) UDPRelease(slot uint8)                  { utility.UDPRelease(slot) }

func (network) TCPCall(request *tcpapi.Request, deadline uint64, reply *tcpapi.Reply) bool {
	wait := ticks(deadline)
	return wait != 0 && utility.TCPCall(request, wait, reply)
}

func (network) TCPPayload() []byte                     { return utility.TCPPayload() }
func (network) TCPReceived(reply *tcpapi.Reply) []byte { return utility.TCPReceived(reply) }
func (network) TCPRelease(slot uint8)                  { utility.TCPRelease(slot) }

// terminal is standard output. Everything dnsq prints from a response is
// already escaped to printable ASCII.
type terminal struct{}

func (terminal) Byte(character byte) {
	one[0] = character
	utility.Print(one[:])
}

func (terminal) Text(text string) { utility.PrintText(text) }
func (terminal) Line()            { utility.EndLine() }
