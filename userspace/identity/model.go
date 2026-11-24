package identity

import "crypto/sha256"

// Key is a 32-byte Ed25519 public key (crypto/ed25519.PublicKeySize).
type Key [KeySize]byte

// Fingerprint returns the SHA-256 digest of the raw public key bytes. It is
// both the key-removal handle and the sort key for a record's keys.
func Fingerprint(k Key) [FingerprintSize]byte {
	return sha256.Sum256(k[:])
}

// Account is a stable account record. Name and Home are stored as fixed-size
// arrays plus a length byte; this keeps records value-comparable and the
// snapshot codec deterministic without allocating on the encode path. The
// arrays are never indexed beyond their length field.
type Account struct {
	ID         uint64 // stable AccountID; never reused
	Name       [MaxName]byte
	NameLen    uint8
	UID        uint32
	PrimaryGID uint64 // internal GroupID of the primary group
	Groups     [MaxSupplementaryGroups]uint64
	GroupCount uint8
	Home       [MaxHome]byte
	HomeLen    uint8
	Enabled    bool
	Keys       [MaxKeysPerAccount]Key
	KeyCount   uint8
}

// NameString returns the account name.
func (a *Account) NameString() string { return string(a.Name[:a.NameLen]) }

// HomePath returns the home path.
func (a *Account) HomePath() string { return string(a.Home[:a.HomeLen]) }

// Group is a stable group record.
type Group struct {
	ID      uint64 // stable GroupID; never reused
	Name    [MaxName]byte
	NameLen uint8
	GID     uint32
}

// NameString returns the group name.
func (g *Group) NameString() string { return string(g.Name[:g.NameLen]) }

// Store owns the accounts, groups, epoch, and monotonic allocation state. It
// is a single mutation owner: every write goes through a Store method.
type Store struct {
	accounts      []Account
	groups        []Group
	epoch         [16]byte // 128-bit database identity; zero only for in-memory tests
	nextAccountID uint64   // monotonic; never reused
	nextGroupID   uint64   // monotonic; never reused
	nextUID       uint64   // UID high-water: next value to allocate
	nextGID       uint64   // GID high-water: next value to allocate
	sequence      uint64   // committed mutation sequence
}

// NewStore returns an empty store ready for its first mutation with a zero
// epoch. The first allocated AccountID and GroupID are 1; 0 is never a valid
// internal ID. A zero-epoch store is for in-memory tests only: it cannot be
// encoded into a snapshot that Decode accepts. Use NewStoreWithEpoch for any
// persistent store.
func NewStore() *Store {
	return &Store{
		nextAccountID: 1,
		nextGroupID:   1,
		nextUID:       uint64(AllocBase),
		nextGID:       uint64(AllocBase),
	}
}

// NewStoreWithEpoch returns an empty store carrying the given 128-bit database
// epoch, ready for persistence. The epoch is supplied by the trusted
// provisioning/recovery path; the library stores it verbatim and Decode
// refuses a zero epoch.
func NewStoreWithEpoch(epoch [16]byte) *Store {
	return &Store{
		epoch:         epoch,
		nextAccountID: 1,
		nextGroupID:   1,
		nextUID:       uint64(AllocBase),
		nextGID:       uint64(AllocBase),
	}
}

// Epoch returns the store's 128-bit database epoch. The canonical identity
// reference for an account or group is (epoch, ID).
func (s *Store) Epoch() [16]byte {
	if s == nil {
		return [16]byte{}
	}
	return s.epoch
}

// Clone returns a deep copy of the store that shares no mutable slices or
// arrays with the receiver. It is used to build candidate state before a
// mutation is validated, encoded, and durably committed.
func (s *Store) Clone() *Store {
	if s == nil {
		return nil
	}
	c := *s
	c.accounts = append([]Account(nil), s.accounts...)
	c.groups = append([]Group(nil), s.groups...)
	return &c
}

// Sequence returns the committed mutation sequence.
func (s *Store) Sequence() uint64 {
	if s == nil {
		return 0
	}
	return s.sequence
}
