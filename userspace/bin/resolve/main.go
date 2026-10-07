//go:build goos_userspace

// Command resolve asks the resolver service for a host name's IPv4
// addresses, shows its status, or flushes or reloads it (ADR 0194). Only the
// exact words status, flush and reload are subcommands; a host with one of
// those names is reached as "status." and so on.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/resolvapi"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "resolve NAME|status|flush|reload",
	Summary: "resolve a host name to IPv4 addresses, or show, flush or reload the resolver",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantResolveAdmin,
}

var (
	text    [320]byte
	request resolvapi.Request
	reply   resolvapi.Reply
)

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	argument, _ := data.Arg(1)
	switch {
	case utility.Equal(argument, "status"):
		status()
	case utility.Equal(argument, "flush"):
		simple(resolvapi.OpFlush, "cache flushed")
	case utility.Equal(argument, "reload"):
		simple(resolvapi.OpReload, "resolver configuration applied")
	default:
		resolve(argument)
	}
}

func call(opcode resolvapi.Opcode) {
	request.Opcode = opcode
	if !utility.CallResolver(&request, &reply) {
		utility.Fail(nil, "resolver service unavailable")
	}
}

func resolve(argument []byte) {
	name, valid := hostname.Parse(argument)
	if !valid {
		utility.Fail(argument, "invalid host name")
	}
	request.Name = name
	call(resolvapi.OpResolve)
	if reply.Result != resolvapi.ResultOK {
		utility.Fail(name.Bytes(), reply.Result.Text())
	}
	line := append(text[:0], name.Bytes()...)
	line = append(line, ':')
	for index := 0; index < int(reply.Count); index++ {
		line = append(line, ' ')
		line = network.AppendIPv4(line, reply.Addresses[index])
	}
	line = append(line, " ("...)
	line = append(line, reply.Source.Text()...)
	if reply.TTL != 0 {
		line = append(line, ", ttl "...)
		line = appendDecimal(line, uint64(reply.TTL))
		line = append(line, " s"...)
	}
	line = append(line, ')')
	utility.Print(line)
	utility.EndLine()
}

func simple(opcode resolvapi.Opcode, done string) {
	call(opcode)
	switch reply.Result {
	case resolvapi.ResultOK:
		utility.PrintText(done)
		utility.EndLine()
	case resolvapi.ResultConfigInvalid:
		reason := append(text[:0], "invalid setting "...)
		reason = append(reason, resolvapi.Key(reply.Field)...)
		reason = append(reason, "; previous configuration kept"...)
		utility.Fail(nil, string(reason))
	default:
		utility.Fail(nil, reply.Result.Text())
	}
}

func status() {
	call(resolvapi.OpStatus)
	if reply.Result != resolvapi.ResultOK {
		utility.Fail(nil, reply.Result.Text())
	}
	servers := text[:0]
	if reply.Count == 0 {
		servers = append(servers, "none"...)
	}
	for index := 0; index < int(reply.Count); index++ {
		if index > 0 {
			servers = append(servers, ", "...)
		}
		servers = network.AppendIPv4(servers, reply.Servers[index].Address)
		servers = append(servers, " ("...)
		servers = append(servers, reply.Servers[index].Source.Text()...)
		servers = append(servers, ')')
	}
	field("servers", servers)
	cache := appendDecimal(text[:0], uint64(reply.CacheUsed))
	cache = append(cache, '/')
	cache = appendDecimal(cache, uint64(reply.CacheLimit))
	cache = append(cache, " positive "...)
	cache = appendDecimal(cache, uint64(reply.Positive))
	cache = append(cache, " negative "...)
	cache = appendDecimal(cache, uint64(reply.Negative))
	cache = append(cache, " failures "...)
	cache = appendDecimal(cache, uint64(reply.Failures))
	field("cache", cache)
	counter("hits", "positive", reply.PositiveHits, "negative", reply.NegativeHits, "failure", reply.FailureHits)
	counter("upstream", "misses", reply.Misses, "queries", reply.Queries, "timeouts", reply.Timeouts)
	counter("dropped", "malformed", reply.Malformed, "mismatched", reply.Mismatched, "truncated", reply.Truncated)
	counter("errors", "server", reply.ServerFailures, "", 0, "", 0)
	field("generation", appendDecimal(text[:0], reply.Generation))
	utility.Mark("RESOLVE_STATUS_OK\n")
}

func counter(name, first string, firstValue uint64, second string, secondValue uint64, third string, thirdValue uint64) {
	line := append(text[:0], first...)
	line = append(line, ' ')
	line = appendDecimal(line, firstValue)
	if second != "" {
		line = append(line, ' ')
		line = append(line, second...)
		line = append(line, ' ')
		line = appendDecimal(line, secondValue)
	}
	if third != "" {
		line = append(line, ' ')
		line = append(line, third...)
		line = append(line, ' ')
		line = appendDecimal(line, thirdValue)
	}
	field(name, line)
}

func field(name string, value []byte) {
	utility.PrintText(name)
	utility.PrintText(": ")
	utility.Print(value)
	utility.EndLine()
}

func appendDecimal(target []byte, value uint64) []byte {
	if value >= 10 {
		target = appendDecimal(target, value/10)
	}
	return append(target, byte('0'+value%10))
}
