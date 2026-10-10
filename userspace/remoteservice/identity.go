package remoteservice

import (
	"crypto/ed25519"
	"goos/userspace/remoteapi"
)

const identitySlots = remoteapi.MaxWorkers + 2

type hostIdentity struct {
	private           [ed25519.PrivateKeySize]byte
	public            [ed25519.PublicKeySize]byte
	occupied, current bool
	workers           uint8
}

// Identities retains one controller-owned key per referenced host identity.
// Two spare slots cover the current identity and an uncommitted replacement.
type Identities struct{ slots [identitySlots]hostIdentity }

func (p *Identities) Reserve(key ed25519.PrivateKey) (int, bool) {
	if len(key) != ed25519.PrivateKeySize {
		return 0, false
	}
	public := key.Public().(ed25519.PublicKey)
	for i := range p.slots {
		s := &p.slots[i]
		if s.occupied && string(s.public[:]) == string(public) {
			return i, true
		}
	}
	for i := range p.slots {
		s := &p.slots[i]
		if !s.occupied {
			copy(s.private[:], key)
			copy(s.public[:], public)
			s.occupied = true
			return i, true
		}
	}
	return 0, false
}

func (p *Identities) Commit(index int) bool {
	if !p.valid(index) {
		return false
	}
	for i := range p.slots {
		p.slots[i].current = i == index
		p.collect(i)
	}
	return true
}

func (p *Identities) Current() (int, bool) {
	for i := range p.slots {
		if p.slots[i].occupied && p.slots[i].current {
			return i, true
		}
	}
	return 0, false
}

func (p *Identities) Retain(index int) bool {
	if !p.valid(index) {
		return false
	}
	total := 0
	for i := range p.slots {
		total += int(p.slots[i].workers)
	}
	if total >= remoteapi.MaxWorkers {
		return false
	}
	p.slots[index].workers++
	return true
}

func (p *Identities) Release(index int) bool {
	if !p.valid(index) || p.slots[index].workers == 0 {
		return false
	}
	p.slots[index].workers--
	p.collect(index)
	return true
}

// Discard drops an unreferenced candidate, leaving reused identities intact.
func (p *Identities) Discard(index int) {
	if p.valid(index) {
		p.collect(index)
	}
}

func (p *Identities) Sign(index int, data []byte) (signature [ed25519.SignatureSize]byte, ok bool) {
	if !p.valid(index) || p.slots[index].workers == 0 || len(data) != 32 {
		return signature, false
	}
	copy(signature[:], ed25519.Sign(ed25519.PrivateKey(p.slots[index].private[:]), data))
	return signature, true
}

func (p *Identities) valid(index int) bool {
	return index >= 0 && index < len(p.slots) && p.slots[index].occupied
}
func (p *Identities) collect(index int) {
	s := &p.slots[index]
	if s.occupied && !s.current && s.workers == 0 {
		*s = hostIdentity{}
	}
}
