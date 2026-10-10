// Package remoteservice owns bounded ciphertext queues and remote policy commits.
package remoteservice

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"goos/userspace/config"
	"goos/userspace/remoteapi"
	"goos/userspace/remotepolicy"
	"strconv"
)

const QueueSize = 16 * 1024

// Queue never grows and never overwrites bytes awaiting delivery.
type Queue struct {
	data        [QueueSize]byte
	start, size int
}

func (q *Queue) Len() int  { return q.size }
func (q *Queue) Free() int { return len(q.data) - q.size }
func (q *Queue) Put(data []byte) bool {
	if len(data) > q.Free() {
		return false
	}
	for _, value := range data {
		q.data[(q.start+q.size)%len(q.data)] = value
		q.size++
	}
	return true
}
func (q *Queue) Peek(limit int) []byte {
	n := min(q.size, len(q.data)-q.start, limit)
	return q.data[q.start : q.start+n]
}
func (q *Queue) Drop(n int) bool {
	if n < 0 || n > q.size {
		return false
	}
	clear(q.Peek(n))
	left := n - min(n, len(q.data)-q.start)
	if left > 0 {
		clear(q.data[:left])
	}
	q.start = (q.start + n) % len(q.data)
	q.size -= n
	return true
}

// Snapshot is copied for each worker; old connections retain their own policy.
type Snapshot struct {
	Policy      remotepolicy.Policy
	Worker      remoteapi.WorkerPolicy
	Fingerprint string
	Listener    uint32
}

type State struct{ Current *Snapshot }

// CoherentLoad detects changes across two complete reads. It is a bounded
// consistency check, not a transaction: callers with revisions should guard
// the reads with that revision as well.
func CoherentLoad(get remotepolicy.Getter) (remotepolicy.Policy, error) {
	values := make(map[string]config.Value, 13)
	first, err := remotepolicy.Load(func(key string) (config.Value, config.Status) {
		v, s := get(key)
		if s == config.StatusOK {
			values[key] = v
		}
		return v, s
	})
	if err != nil {
		return remotepolicy.Policy{}, err
	}
	_, err = remotepolicy.Load(func(key string) (config.Value, config.Status) {
		v, s := get(key)
		if old, ok := values[key]; !ok || old != v {
			return config.Value{}, config.StatusInvalidArgument
		}
		return v, s
	})
	if err != nil {
		return remotepolicy.Policy{}, errors.New("remote configuration changed during reload")
	}
	return first, nil
}

// Prepare validates keys and constructs a new generation before touching a
// listener. Empty authorized_keys is valid only while disabled.
func (s *State) Prepare(policy remotepolicy.Policy, host *remotepolicy.HostKey, keys []ed25519.PublicKey) (*Snapshot, error) {
	if host == nil || len(host.Private) != ed25519.PrivateKeySize || len(keys) > remoteapi.MaxKeys || policy.Enabled && len(keys) == 0 {
		return nil, errors.New("remote keys unavailable")
	}
	generation := uint64(1)
	if s.Current != nil {
		generation = s.Current.Worker.Generation + 1
		if generation == 0 {
			return nil, errors.New("remote policy generation exhausted")
		}
	}
	next := &Snapshot{Policy: policy, Fingerprint: host.Fingerprint, Worker: remoteapi.WorkerPolicy{Generation: generation, MaxSessions: policy.MaxSessions, MaxAuthAttempts: policy.MaxAuthAttempts, HandshakeTimeoutMS: policy.HandshakeTimeoutMS, IdleTimeoutSeconds: policy.IdleTimeoutSec, KeyCount: uint8(len(keys))}}
	copy(next.Worker.Seed[:], host.Private.Seed())
	for i, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			return nil, errors.New("invalid authorized key")
		}
		copy(next.Worker.Keys[i][:], key)
	}
	if !next.Worker.Valid() {
		return nil, errors.New("invalid remote worker policy")
	}
	return next, nil
}
func SameBind(a, b remotepolicy.Policy) bool {
	return a.Enabled && b.Enabled && a.ListenAddress == b.ListenAddress && a.Port == b.Port
}
func (s *State) Commit(next *Snapshot) bool {
	if next == nil || !next.Worker.Valid() || next.Policy.Enabled && next.Listener == 0 || !next.Policy.Enabled && next.Listener != 0 {
		return false
	}
	if s.Current != nil && next.Worker.Generation != s.Current.Worker.Generation+1 {
		return false
	}
	s.Current = next
	return true
}
func (s *State) Status(pending, authenticated int) []byte {
	if s.Current == nil {
		return []byte("service=unavailable listener=closed\n")
	}
	current := s.Current
	p := current.Policy
	text := "service=running enabled=" + strconv.FormatBool(p.Enabled) + " listener="
	if current.Listener == 0 {
		text += "closed"
	} else {
		text += "listening"
	}
	text += " bind=" + strconv.Itoa(int(p.ListenAddress[0])) + "." + strconv.Itoa(int(p.ListenAddress[1])) + "." + strconv.Itoa(int(p.ListenAddress[2])) + "." + strconv.Itoa(int(p.ListenAddress[3])) + ":" + strconv.Itoa(int(p.Port))
	text += " sessions=" + strconv.Itoa(authenticated) + " pending=" + strconv.Itoa(pending) + " fingerprint=" + current.Fingerprint
	text += " max-sessions=" + strconv.Itoa(int(p.MaxSessions)) + " settings=configured"
	text += " authorized-keys=" + p.AuthorizedKeys + " key-count=" + strconv.Itoa(int(current.Worker.KeyCount))
	text += " kex=" + remotepolicy.KEXAlgorithm + " cipher=" + remotepolicy.CipherAlgorithm + " host-key=" + remotepolicy.KeyAlgorithm + " user-key=" + remotepolicy.KeyAlgorithm + " source=system.remote generation=" + strconv.FormatUint(current.Worker.Generation, 10) + "\n"
	return []byte(text)
}
func SeedEqual(a, b *Snapshot) bool {
	return a != nil && b != nil && bytes.Equal(a.Worker.Seed[:], b.Worker.Seed[:])
}
