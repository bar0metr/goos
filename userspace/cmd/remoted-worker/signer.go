package main

import (
	"crypto/ed25519"
	"errors"
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/remoteapi"
)

var errHostSigner = errors.New("remote host signer unavailable")

type signerTransport struct {
	send         func(*ipc.Message) bool
	receive      func(uint64, *ipc.Message) kcall.Status
	closeHandles func(*ipc.Message)
	close        func()
	now          func() uint64
}
type hostSigner struct {
	public                         [ed25519.PublicKeySize]byte
	generation, sequence, deadline uint64
	failed, inFlight               bool
	transport                      signerTransport
}

func newHostSigner(public ed25519.PublicKey, generation uint64, transport signerTransport) (*hostSigner, bool) {
	if len(public) != ed25519.PublicKeySize || generation == 0 || transport.send == nil || transport.receive == nil || transport.closeHandles == nil || transport.close == nil || transport.now == nil {
		return nil, false
	}
	signer := &hostSigner{generation: generation, transport: transport}
	copy(signer.public[:], public)
	return signer, true
}
func (s *hostSigner) fail() ([]byte, error) {
	if !s.failed {
		s.failed = true
		s.transport.close()
	}
	return nil, errHostSigner
}
func (s *hostSigner) Sign(data []byte) ([]byte, error) {
	now := s.transport.now()
	deadline := now + 5*clock.TicksPerSecond
	if deadline < now {
		return s.fail()
	}
	if s.deadline != 0 && s.deadline < deadline {
		deadline = s.deadline
	}
	if s.failed || s.inFlight || len(data) != 32 || s.sequence == ^uint64(0) || now >= deadline {
		return s.fail()
	}
	s.inFlight = true
	var request remoteapi.SignRequest
	var outgoing, incoming ipc.Message
	var reply remoteapi.SignReply
	defer func() {
		s.transport.closeHandles(&incoming)
		clear(request.Data[:])
		clear(outgoing.Payload[:])
		clear(incoming.Payload[:])
		clear(reply.Signature[:])
		s.inFlight = false
	}()
	request.Generation = s.generation
	copy(request.Data[:], data)
	correlation := s.sequence + 1
	if !remoteapi.EncodeSignRequest(&request, correlation, &outgoing) || !s.transport.send(&outgoing) || s.failed {
		return s.fail()
	}
	now = s.transport.now()
	if now >= deadline || s.transport.receive(deadline-now, &incoming) != kcall.StatusOK || s.transport.now() >= deadline || s.failed {
		return s.fail()
	}
	if !remoteapi.DecodeSignReply(&incoming, correlation, s.generation, &reply) || reply.Result != remoteapi.ResultOK || !ed25519.Verify(s.public[:], data, reply.Signature[:]) {
		return s.fail()
	}
	result := make([]byte, ed25519.SignatureSize)
	copy(result, reply.Signature[:])
	s.sequence = correlation
	return result, nil
}
