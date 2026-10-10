package remoteapi

import "encoding/binary"

const (
	PolicyPageSize   = 4096
	MaxKeys          = 64
	MaxUsername      = 32
	policyHeaderSize = 104
	policyVersion    = 2
)

// WorkerPolicy is a public, immutable per-connection snapshot.
type WorkerPolicy struct {
	Generation         uint64
	MaxSessions        uint8
	MaxAuthAttempts    uint8
	HandshakeTimeoutMS uint32
	IdleTimeoutSeconds uint32
	HostPublicKey      [32]byte
	Username           [MaxUsername]byte
	UserLen            uint8
	Keys               [MaxKeys][32]byte
	KeyCount           uint8
}

func ValidUsername(user []byte) bool {
	if len(user) == 0 || len(user) > MaxUsername {
		return false
	}
	for _, c := range user {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (policy *WorkerPolicy) Valid() bool {
	return policy.Generation != 0 && policy.MaxSessions >= 1 && policy.MaxSessions <= MaxSessions && policy.MaxAuthAttempts >= 1 && policy.MaxAuthAttempts <= 10 && policy.HandshakeTimeoutMS >= 1000 && policy.HandshakeTimeoutMS <= 120000 && policy.IdleTimeoutSeconds <= 86400 && int(policy.KeyCount) <= len(policy.Keys) && policy.UserLen <= MaxUsername && ValidUsername(policy.Username[:policy.UserLen]) && zero(policy.Username[policy.UserLen:]) && !zero(policy.HostPublicKey[:])
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
	page[8], page[9], page[10], page[11] = policy.KeyCount, policy.MaxSessions, policy.MaxAuthAttempts, policy.UserLen
	binary.LittleEndian.PutUint32(page[12:16], policy.HandshakeTimeoutMS)
	binary.LittleEndian.PutUint32(page[16:20], policy.IdleTimeoutSeconds)
	binary.LittleEndian.PutUint64(page[24:32], policy.Generation)
	copy(page[32:64], policy.HostPublicKey[:])
	copy(page[64:96], policy.Username[:])
	for index := 0; index < int(policy.KeyCount); index++ {
		copy(page[policyHeaderSize+index*32:], policy.Keys[index][:])
	}
	return true
}

func DecodePolicy(page []byte, out *WorkerPolicy) bool {
	if len(page) != PolicyPageSize || string(page[:4]) != "GSP1" || binary.LittleEndian.Uint16(page[4:6]) != policyVersion || !zero(page[20:24]) || !zero(page[96:104]) || page[8] > MaxKeys {
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
	userSize := int(page[11])
	if generation == 0 || page[9] < 1 || page[9] > MaxSessions || page[10] < 1 || page[10] > 10 || handshake < 1000 || handshake > 120000 || idle > 86400 || userSize > MaxUsername || !ValidUsername(page[64:64+userSize]) || !zero(page[64+userSize:96]) || zero(page[32:64]) {
		return false
	}
	out.Generation, out.MaxSessions, out.MaxAuthAttempts = generation, page[9], page[10]
	out.HandshakeTimeoutMS, out.IdleTimeoutSeconds, out.KeyCount, out.UserLen = handshake, idle, page[8], page[11]
	copy(out.HostPublicKey[:], page[32:64])
	copy(out.Username[:], page[64:96])
	for index := range out.Keys {
		clear(out.Keys[index][:])
	}
	for index := 0; index < count; index++ {
		copy(out.Keys[index][:], page[policyHeaderSize+32*index:policyHeaderSize+32*(index+1)])
	}
	return true
}
