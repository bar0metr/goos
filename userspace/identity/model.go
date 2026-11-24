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

// Store owns the accounts, groups, and monotonic allocation state. It is a
// single mutation owner: every write goes through a Store method.
type Store struct {
	accounts      []Account
	groups        []Group
	nextAccountID uint64
	nextGroupID   uint64
	sequence      uint64
}

// NewStore returns an empty store ready for its first mutation. The first
// allocated AccountID and GroupID are 1; 0 is never a valid internal ID.
func NewStore() *Store {
	return &Store{nextAccountID: 1, nextGroupID: 1}
}

// Sequence returns the committed mutation sequence.
func (s *Store) Sequence() uint64 {
	if s == nil {
		return 0
	}
	return s.sequence
}
