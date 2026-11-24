package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
)

// Canonical snapshot layout (all multi-byte fields little-endian):
//
//	magic   "GOOS-IDENTITY-V1\n" (17 bytes)
//	version uint16 = 1
//	sequence uint64 (monotonic, increments each committed mutation)
//	payload canonical encoding of accounts, groups, nextAccountID, nextGroupID
//	digest  sha256(magic || version || sequence || payload) (32 bytes)
const (
	snapshotMagic    = "GOOS-IDENTITY-V1\n"
	snapshotVersion  = uint16(1)
	magicSize        = len(snapshotMagic) // 17
	versionSize      = 2
	sequenceSize     = 8
	digestSize       = 32
	snapshotHeaderSz = magicSize + versionSize + sequenceSize
)

func appendU16(b []byte, v uint16) []byte {
	var t [2]byte
	binary.LittleEndian.PutUint16(t[:], v)
	return append(b, t[:]...)
}

func appendU32(b []byte, v uint32) []byte {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	return append(b, t[:]...)
}

func appendU64(b []byte, v uint64) []byte {
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], v)
	return append(b, t[:]...)
}

// Encode writes the store as the canonical snapshot. It encodes the store's
// current state deterministically without validating it; the mutation methods
// keep a store canonical, and Decode/Validate enforce invariants on load.
func Encode(store *Store) ([]byte, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	payload, err := encodePayload(store)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, snapshotHeaderSz+len(payload)+digestSize)
	out = append(out, snapshotMagic...)
	out = appendU16(out, snapshotVersion)
	out = appendU64(out, store.sequence)
	out = append(out, payload...)
	digest := sha256.Sum256(out)
	out = append(out, digest[:]...)
	if len(out) > MaxSnapshotBytes {
		return nil, ErrExhausted
	}
	return out, nil
}

func encodePayload(s *Store) ([]byte, error) {
	if len(s.accounts) > MaxAccounts || len(s.groups) > MaxGroups {
		return nil, ErrInvalid
	}
	var out []byte
	out = appendU32(out, uint32(len(s.accounts)))
	out = appendU32(out, uint32(len(s.groups)))
	out = appendU64(out, s.nextAccountID)
	out = appendU64(out, s.nextGroupID)
	for i := range s.accounts {
		out = encodeAccount(out, &s.accounts[i])
	}
	for i := range s.groups {
		out = encodeGroup(out, &s.groups[i])
	}
	return out, nil
}

func encodeAccount(out []byte, a *Account) []byte {
	out = appendU64(out, a.ID)
	out = append(out, a.NameLen)
	out = append(out, a.Name[:a.NameLen]...)
	out = appendU32(out, a.UID)
	out = appendU64(out, a.PrimaryGID)
	out = append(out, a.GroupCount)
	for i := 0; i < int(a.GroupCount); i++ {
		out = appendU64(out, a.Groups[i])
	}
	out = append(out, a.HomeLen)
	out = append(out, a.Home[:a.HomeLen]...)
	var enabled byte
	if a.Enabled {
		enabled = 1
	}
	out = append(out, enabled)
	out = append(out, a.KeyCount)
	for i := 0; i < int(a.KeyCount); i++ {
		out = append(out, a.Keys[i][:]...)
	}
	return out
}

func encodeGroup(out []byte, g *Group) []byte {
	out = appendU64(out, g.ID)
	out = append(out, g.NameLen)
	out = append(out, g.Name[:g.NameLen]...)
	out = appendU32(out, g.GID)
	return out
}

// Decode parses and fully validates a snapshot, returning a fresh Store.
func Decode(data []byte) (*Store, error) {
	if len(data) > MaxSnapshotBytes {
		return nil, ErrInvalid
	}
	if len(data) < snapshotHeaderSz+digestSize {
		return nil, ErrInvalid
	}
	if !bytes.Equal(data[:magicSize], []byte(snapshotMagic)) {
		return nil, ErrInvalid
	}
	if binary.LittleEndian.Uint16(data[magicSize:magicSize+versionSize]) != snapshotVersion {
		return nil, ErrInvalid
	}
	sequence := binary.LittleEndian.Uint64(data[magicSize+versionSize : snapshotHeaderSz])
	body := data[:len(data)-digestSize]
	expected := sha256.Sum256(body)
	if !bytes.Equal(expected[:], data[len(data)-digestSize:]) {
		return nil, ErrInvalid
	}
	return decodePayload(body[snapshotHeaderSz:], sequence)
}

// Validate verifies magic, version, digest, then payload bounds, duplicates,
// and dangling references, without returning a store.
func Validate(data []byte) error {
	_, err := Decode(data)
	return err
}

func decodePayload(payload []byte, sequence uint64) (*Store, error) {
	d := decoder{b: payload}
	accountCount, err := d.u32()
	if err != nil {
		return nil, err
	}
	groupCount, err := d.u32()
	if err != nil {
		return nil, err
	}
	nextAccountID, err := d.u64()
	if err != nil {
		return nil, err
	}
	nextGroupID, err := d.u64()
	if err != nil {
		return nil, err
	}
	if accountCount > MaxAccounts || groupCount > MaxGroups {
		return nil, ErrInvalid
	}
	s := &Store{nextAccountID: nextAccountID, nextGroupID: nextGroupID, sequence: sequence}
	s.accounts = make([]Account, 0, int(accountCount))
	s.groups = make([]Group, 0, int(groupCount))
	for i := uint32(0); i < accountCount; i++ {
		var a Account
		if a.ID, err = d.u64(); err != nil {
			return nil, err
		}
		if a.NameLen, err = d.u8(); err != nil {
			return nil, err
		}
		if a.NameLen == 0 || a.NameLen > MaxName {
			return nil, ErrInvalid
		}
		name, err := d.take(int(a.NameLen))
		if err != nil {
			return nil, err
		}
		copy(a.Name[:], name)
		if a.UID, err = d.u32(); err != nil {
			return nil, err
		}
		if a.PrimaryGID, err = d.u64(); err != nil {
			return nil, err
		}
		if a.GroupCount, err = d.u8(); err != nil {
			return nil, err
		}
		if a.GroupCount > MaxSupplementaryGroups {
			return nil, ErrInvalid
		}
		for j := uint8(0); j < a.GroupCount; j++ {
			gid, err := d.u64()
			if err != nil {
				return nil, err
			}
			a.Groups[j] = gid
		}
		if a.HomeLen, err = d.u8(); err != nil {
			return nil, err
		}
		if a.HomeLen > MaxHome {
			return nil, ErrInvalid
		}
		home, err := d.take(int(a.HomeLen))
		if err != nil {
			return nil, err
		}
		copy(a.Home[:], home)
		var enabled byte
		if enabled, err = d.u8(); err != nil {
			return nil, err
		}
		if enabled > 1 {
			return nil, ErrInvalid
		}
		a.Enabled = enabled == 1
		if a.KeyCount, err = d.u8(); err != nil {
			return nil, err
		}
		if a.KeyCount > MaxKeysPerAccount {
			return nil, ErrInvalid
		}
		for j := uint8(0); j < a.KeyCount; j++ {
			kb, err := d.take(KeySize)
			if err != nil {
				return nil, err
			}
			copy(a.Keys[j][:], kb)
		}
		s.accounts = append(s.accounts, a)
	}
	for i := uint32(0); i < groupCount; i++ {
		var g Group
		if g.ID, err = d.u64(); err != nil {
			return nil, err
		}
		if g.NameLen, err = d.u8(); err != nil {
			return nil, err
		}
		if g.NameLen == 0 || g.NameLen > MaxName {
			return nil, ErrInvalid
		}
		name, err := d.take(int(g.NameLen))
		if err != nil {
			return nil, err
		}
		copy(g.Name[:], name)
		if g.GID, err = d.u32(); err != nil {
			return nil, err
		}
		s.groups = append(s.groups, g)
	}
	if err := d.done(); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

type decoder struct {
	b   []byte
	off int
}

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || d.off+n > len(d.b) {
		return nil, ErrInvalid
	}
	s := d.b[d.off : d.off+n]
	d.off += n
	return s, nil
}

func (d *decoder) u8() (byte, error) {
	b, err := d.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *decoder) u32() (uint32, error) {
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (d *decoder) u64() (uint64, error) {
	b, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (d *decoder) done() error {
	if d.off != len(d.b) {
		return ErrInvalid
	}
	return nil
}
