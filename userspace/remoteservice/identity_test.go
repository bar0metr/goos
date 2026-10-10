package remoteservice

import (
	"crypto/ed25519"
	"goos/userspace/remoteapi"
	"testing"
)

func identityKey(value byte) ed25519.PrivateKey {
	var seed [32]byte
	seed[0] = value
	return ed25519.NewKeyFromSeed(seed[:])
}

func TestIdentitySharingReloadRekeyAndZeroing(t *testing.T) {
	var pool Identities
	aKey, bKey := identityKey(1), identityKey(2)
	a, ok := pool.Reserve(aKey)
	if !ok || !pool.Commit(a) || !pool.Retain(a) || !pool.Retain(a) {
		t.Fatal("first identity")
	}
	same, ok := pool.Reserve(aKey)
	if !ok || same != a {
		t.Fatal("duplicated identity")
	}
	pool.Discard(same)
	if !pool.valid(a) {
		t.Fatal("discarded referenced identity")
	}
	b, ok := pool.Reserve(bKey)
	if !ok || a == b || !pool.Commit(b) || !pool.Retain(b) {
		t.Fatal("replacement identity")
	}
	var exchange [32]byte
	exchange[0] = 7
	sig, ok := pool.Sign(a, exchange[:])
	if !ok || !ed25519.Verify(aKey.Public().(ed25519.PublicKey), exchange[:], sig[:]) || ed25519.Verify(bKey.Public().(ed25519.PublicKey), exchange[:], sig[:]) {
		t.Fatal("old rekey identity")
	}
	if !pool.Release(a) || !pool.valid(a) {
		t.Fatal("premature zeroing")
	}
	if !pool.Release(a) || pool.slots[a] != (hostIdentity{}) {
		t.Fatal("retired key not zeroed")
	}
	if _, ok := pool.Sign(a, exchange[:]); ok {
		t.Fatal("retired signer")
	}
	if !pool.Release(b) || !pool.valid(b) {
		t.Fatal("current identity discarded")
	}
	if _, ok := pool.Sign(b, exchange[:]); ok {
		t.Fatal("signed without worker reference")
	}
}

func TestIdentityCapacityIncludesStaging(t *testing.T) {
	var pool Identities
	for i := 0; i < remoteapi.MaxWorkers; i++ {
		index, ok := pool.Reserve(identityKey(byte(i + 1)))
		if !ok || !pool.Commit(index) || !pool.Retain(index) {
			t.Fatal("worker identity", i)
		}
	}
	current, ok := pool.Reserve(identityKey(100))
	if !ok || !pool.Commit(current) {
		t.Fatal("current identity")
	}
	candidate, ok := pool.Reserve(identityKey(101))
	if !ok {
		t.Fatal("no staging slot at worker capacity")
	}
	if _, ok := pool.Reserve(identityKey(102)); ok {
		t.Fatal("unbounded identity pool")
	}
	if pool.Retain(candidate) {
		t.Fatal("worker capacity bypass")
	}
	pool.Discard(candidate)
	if pool.slots[candidate] != (hostIdentity{}) {
		t.Fatal("staging key not cleared")
	}
	if actual, ok := pool.Current(); !ok || actual != current {
		t.Fatal("failed candidate changed current")
	}
	candidate, ok = pool.Reserve(identityKey(102))
	if !ok || !pool.Commit(candidate) || pool.slots[current] != (hostIdentity{}) {
		t.Fatal("replacement at full capacity")
	}
}

func TestIdentityInvalidOperations(t *testing.T) {
	var pool Identities
	if _, ok := pool.Reserve(nil); ok {
		t.Fatal("nil key")
	}
	if pool.Commit(-1) || pool.Retain(0) || pool.Release(0) {
		t.Fatal("invalid slot")
	}
	pool.Discard(-1)
	index, _ := pool.Reserve(identityKey(1))
	if _, ok := pool.Sign(index, make([]byte, 31)); ok {
		t.Fatal("invalid signing input")
	}
	pool.Discard(index)
	if pool.slots[index] != (hostIdentity{}) {
		t.Fatal("uncommitted key not cleared")
	}
}
