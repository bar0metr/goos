package remoteservice

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

func TestKeyReloadRetainsHostAndOldSessionsNewAdmissionsUseCommittedKeys(t *testing.T) {
	policy, host, keys := fixture(t)
	policy.Enabled = true
	var state State
	var pool Identities
	identity, ok := pool.Reserve(host.Private)
	if !ok || !pool.Commit(identity) || !pool.Retain(identity) {
		t.Fatal("host pool")
	}
	first, err := state.Prepare(policy, host, keys)
	if err != nil {
		t.Fatal(err)
	}
	first.Listener = 42
	if !state.Commit(first) {
		t.Fatal("first commit")
	}
	retained, err := state.RetainedHost(policy.HostKey)
	if err != nil || len(retained.Private) != 0 || retained.Fingerprint != host.Fingerprint || !bytes.Equal(retained.Public, host.Public) {
		t.Fatal("retained identity", err)
	}
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	second, err := state.Prepare(policy, retained, []ed25519.PublicKey{other})
	if err != nil {
		t.Fatal(err)
	}
	second.Listener = 42
	if !state.Commit(second) || !pool.Commit(identity) {
		t.Fatal("key commit")
	}
	if !bytes.Equal(first.Worker.Keys[0][:], keys[0]) || !bytes.Equal(state.Current.Worker.Keys[0][:], other) {
		t.Fatal("old sessions/new admission policy changed incorrectly")
	}
	if first.Worker.Generation != 1 || second.Worker.Generation != 2 || first.Listener != second.Listener || !IdentityEqual(first, second) {
		t.Fatal("key reload rotated listener or identity")
	}
	hash := bytes.Repeat([]byte{3}, 32)
	signature, ok := pool.Sign(identity, hash)
	if !ok || !ed25519.Verify(host.Public, hash, signature[:]) {
		t.Fatal("old session signer lost")
	}
	empty, err := state.Prepare(policy, retained, nil)
	if err != nil {
		t.Fatal("deny-all", err)
	}
	empty.Listener = 42
	if !state.Commit(empty) || state.Current.Worker.KeyCount != 0 || !bytes.Equal(first.Worker.Keys[0][:], keys[0]) {
		t.Fatal("empty new admissions policy/old sessions")
	}
	if _, err := state.RetainedHost(policy.HostKey + "-other"); err == nil {
		t.Fatal("configured host path changed without explicit reload")
	}
	retained.Public[0] ^= 1
	if bytes.Equal(retained.Public, state.Current.Worker.HostPublicKey[:]) {
		t.Fatal("retained public copy aliases snapshot")
	}
}
func TestKeyReloadRequiresVerifiedCurrentIdentity(t *testing.T) {
	var state State
	if _, err := state.RetainedHost("/state/security/ssh/host"); err == nil {
		t.Fatal("unverified identity accepted")
	}
}
