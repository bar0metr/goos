//go:build goos_userspace

package main

// The local DNS server on 127.0.0.1:53 (userspace/dnsstub) for programs
// that speak DNS themselves, such as a static C library reading
// /etc/resolv.conf. It answers from the same resolver, hosts table and
// cache as the sessions do, and runs on this service's own UDP and TCP
// sessions: every correlation it issues has the top bit set, and replies
// carrying one go to it. Its requests are resolver clients maxSessions
// through maxSessions+dnsstub.MaxRequests-1.
//
// Endpoint budget, against the 8 endpoints netd gives each session:
//
//	UDP: up to dns.MaxTransactions (4) upstream queries + 1 stub endpoint
//	TCP: up to tcpupstream.MaxStreams (2) fallbacks + 1 stub listener +
//	     dnsstub.Backlog (2) unaccepted + dnsstub.MaxConnections (2)
//
// that is 5 and 7 of 8. A fallback the resolver closes first waits out
// TIME_WAIT on the session's count; netd recycles the oldest such slot.

import (
	"goos/userspace/dns"
	"goos/userspace/dnsstub"
	"goos/userspace/hostname"
	"goos/userspace/runtime/sys"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpupstream"
	"goos/userspace/udpapi"
	"goos/userspace/upstream"
)

const stubBase = maxSessions

// The stub's clients follow the sessions' in the resolver's client table.
var _ = [dns.MaxClients - stubBase - dnsstub.MaxRequests]struct{}{}

var stub dnsstub.Server

// stubHosts reads the hosts table for the stub.
type stubHosts struct{}

func (stubHosts) Address(name *hostname.Name) ([4]byte, bool) { return hostsAddress(name) }

func stubInit() { stub.Init(stubBase, stubHosts{}) }

func stubBindUDP(txBase, rxBase uint64) {
	stub.BindUDP(sys.Window(txBase, udpapi.TXSlots*udpapi.SlotSize), sys.Window(rxBase, udpapi.RXSlots*udpapi.SlotSize), &client, clockNanos())
}

func stubBindTCP(txBase, rxBase uint64) {
	stub.BindTCP(sys.Window(txBase, tcpapi.TXSlots*tcpapi.SlotSize), sys.Window(rxBase, tcpapi.RXSlots*tcpapi.SlotSize), &fallback, clockNanos())
}

// stubFlush gives the stub its turn in the service loop.
func stubFlush(now uint64) {
	if link.channel != 0 && !stub.FlushUDP(now, udpSender{}) {
		closeNetwork()
	}
	if stream.channel != 0 && !stub.FlushTCP(&resolver, now, tcpSender{}) {
		closeTCP()
	}
}

// answerUDP routes one UDP reply to the stub or the upstream client.
func answerUDP(answered, now uint64) bool {
	if dnsstub.Owns(answered) {
		return stub.AnswerUDP(&resolver, &udpReply, answered, now, udpSender{})
	}
	return client.Answer(&resolver, &udpReply, answered, now, udpSender{})
}

func answerTCP(answered, now uint64) bool {
	if dnsstub.Owns(answered) {
		return stub.AnswerTCP(&resolver, &tcpReply, answered, now, tcpSender{})
	}
	return fallback.Answer(&resolver, &tcpReply, answered, now, tcpSender{})
}

var (
	_ dnsstub.UDPSender  = udpSender{}
	_ dnsstub.TCPSender  = tcpSender{}
	_ upstream.Sender    = udpSender{}
	_ tcpupstream.Sender = tcpSender{}
)
