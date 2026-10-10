package remoteapi

import "encoding/binary"

const (
	PolicyPageSize   = 4096
	MaxKeys          = 64
	policyHeaderSize = 64
	policyVersion    = 1
)

// WorkerPolicy is an immutable per-connection snapshot. Only the trusted
// controller, PID 1, and its worker receive the seed-bearing memory object.
type WorkerPolicy struct {
	Generation         uint64
	MaxSessions        uint8
	MaxAuthAttempts    uint8
	HandshakeTimeoutMS uint32
	IdleTimeoutSeconds uint32
	Seed               [32]byte
	Keys               [MaxKeys][32]byte
	KeyCount           uint8
}

func (policy *WorkerPolicy) Valid() bool {
	return policy.Generation != 0 && policy.MaxSessions >= 1 && policy.MaxSessions <= MaxSessions && policy.MaxAuthAttempts >= 1 && policy.MaxAuthAttempts <= 10 && policy.HandshakeTimeoutMS >= 1000 && policy.HandshakeTimeoutMS <= 120000 && policy.IdleTimeoutSeconds <= 86400 && int(policy.KeyCount) <= len(policy.Keys)
}

func EncodePolicy(policy *WorkerPolicy, page []byte) bool {
	if len(page) != PolicyPageSize || !policy.Valid() {
		return false
	}
	clear(page)
	copy(page, "GSP1")
	binary.LittleEndian.PutUint16(page[4:6], policyVersion)
	size := policyHeaderSize + 32*int(policy.KeyCount)
	binary.LittleEndian.PutUint16(page[6:8], uint16(size))
	page[8], page[9], page[10] = policy.KeyCount, policy.MaxSessions, policy.MaxAuthAttempts
	binary.LittleEndian.PutUint32(page[12:16], policy.HandshakeTimeoutMS)
	binary.LittleEndian.PutUint32(page[16:20], policy.IdleTimeoutSeconds)
	binary.LittleEndian.PutUint64(page[24:32], policy.Generation)
	copy(page[32:64], policy.Seed[:])
	for index := 0; index < int(policy.KeyCount); index++ {
		copy(page[policyHeaderSize+index*32:], policy.Keys[index][:])
	}
	return true
}

func DecodePolicy(page []byte, out *WorkerPolicy) bool {
	if len(page) != PolicyPageSize || string(page[:4]) != "GSP1" || binary.LittleEndian.Uint16(page[4:6]) != policyVersion || page[11] != 0 || !zero(page[20:24]) || page[8] > MaxKeys {
		return false
	}
	count := int(page[8])
	size := policyHeaderSize + 32*count
	if int(binary.LittleEndian.Uint16(page[6:8])) != size || !zero(page[size:]) {
		return false
	}
	generation := binary.LittleEndian.Uint64(page[24:32])
	handshake := binary.LittleEndian.Uint32(page[12:16])
	idle := binary.LittleEndian.Uint32(page[16:20])
	if generation == 0 || page[9] < 1 || page[9] > MaxSessions || page[10] < 1 || page[10] > 10 || handshake < 1000 || handshake > 120000 || idle > 86400 {
		return false
	}
	out.Generation, out.MaxSessions, out.MaxAuthAttempts = generation, page[9], page[10]
	out.HandshakeTimeoutMS, out.IdleTimeoutSeconds, out.KeyCount = handshake, idle, page[8]
	copy(out.Seed[:], page[32:64])
	for index := range out.Keys {
		clear(out.Keys[index][:])
	}
	for index := 0; index < count; index++ {
		copy(out.Keys[index][:], page[policyHeaderSize+32*index:policyHeaderSize+32*(index+1)])
	}
	return true
}
