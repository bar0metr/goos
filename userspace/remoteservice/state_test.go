package remoteservice

import (
	"bytes"
	"crypto/ed25519"
	"goos/userspace/config"
	"goos/userspace/remotepolicy"
	"strings"
	"testing"
)

func TestQueueBoundedWrap(t *testing.T) {
	var q Queue
	initial := bytes.Repeat([]byte{7}, QueueSize-4)
	if !q.Put(initial) || q.Put(make([]byte, 5)) {
		t.Fatal("queue bounds")
	}
	if !q.Drop(QueueSize-8) || !q.Put([]byte("abcdefgh")) {
		t.Fatal("wrap")
	}
	var got []byte
	for q.Len() > 0 {
		chunk := q.Peek(3)
		got = append(got, chunk...)
		if !q.Drop(len(chunk)) {
			t.Fatal("drop")
		}
	}
	if !bytes.Equal(got, append([]byte{7, 7, 7, 7}, []byte("abcdefgh")...)) {
		t.Fatalf("wrong FIFO %q", got)
	}
	if q.Drop(-1) || q.Drop(1) {
		t.Fatal("invalid drop")
	}
}
func fixture(t *testing.T) (remotepolicy.Policy, *remotepolicy.HostKey, []ed25519.PublicKey) {
	t.Helper()
	var store config.Store
	if remotepolicy.DeclareDefaults(&store) != config.StatusOK {
		t.Fatal("defaults")
	}
	policy, err := remotepolicy.Load(func(key string) (config.Value, config.Status) {
		return store.Get(config.Scope{}, config.Read, []byte(key))
	})
	if err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(make([]byte, 32))
	public := private.Public().(ed25519.PublicKey)
	fingerprint, _ := remotepolicy.Fingerprint(public)
	return policy, &remotepolicy.HostKey{Private: private, Public: public, Fingerprint: fingerprint}, []ed25519.PublicKey{public}
}
func TestCommitPreservesOldWorkerSnapshot(t *testing.T) {
	policy, host, keys := fixture(t)
	var state State
	first, err := state.Prepare(policy, host, keys)
	if err != nil || !state.Commit(first) {
		t.Fatal("first disabled snapshot", err)
	}
	policy.Enabled = true
	second, err := state.Prepare(policy, host, keys)
	if err != nil {
		t.Fatal(err)
	}
	if state.Commit(second) || state.Current != first {
		t.Fatal("committed without ready listener")
	}
	second.Listener = 42
	if !state.Commit(second) {
		t.Fatal("enabled commit")
	}
	if first.Policy.Enabled || first.Worker.Generation != 1 || second.Worker.Generation != 2 || !IdentityEqual(first, second) {
		t.Fatal("snapshot changed")
	}
	if !strings.Contains(string(state.Status(2, 4)), "sessions=4 pending=2") || strings.Contains(string(state.Status(2, 4)), string(host.Private.Seed())) {
		t.Fatal("status")
	}
	third, _ := state.Prepare(policy, host, keys)
	third.Worker.Generation = 1
	third.Listener = 42
	if state.Commit(third) {
		t.Fatal("stale generation")
	}
}
func TestEnabledEmptyKeysRejected(t *testing.T) {
	policy, host, _ := fixture(t)
	var state State
	if _, err := state.Prepare(policy, host, nil); err != nil {
		t.Fatal("disabled empty allowlist", err)
	}
	policy.Enabled = true
	if _, err := state.Prepare(policy, host, nil); err == nil {
		t.Fatal("enabled empty allowlist")
	}
}
func TestCoherentLoadRejectsChangingConfiguration(t *testing.T) {
	var store config.Store
	remotepolicy.DeclareDefaults(&store)
	calls := 0
	_, err := CoherentLoad(func(key string) (config.Value, config.Status) {
		calls++
		if calls == 15 {
			return config.Bool(true), config.StatusOK
		}
		return store.Get(config.Scope{}, config.Read, []byte(key))
	})
	if err == nil {
		t.Fatal("accepted mixed snapshot")
	}
}
