package identity

import (
	"errors"
	"fmt"
)

// Sentinel errors let callers distinguish failure classes without parsing
// messages. Use errors.Is to match them through the context added by Store
// methods.
var (
	ErrNotFound    = errors.New("identity: not found")
	ErrExists      = errors.New("identity: already exists")
	ErrInvalid     = errors.New("identity: invalid")
	ErrExhausted   = errors.New("identity: exhausted")
	ErrConflict    = errors.New("identity: conflict")
	ErrDisabled    = errors.New("identity: disabled")
	ErrUnavailable = errors.New("identity: unavailable")

	// ErrOldFormat reports a snapshot in the obsolete pre-release V1 format.
	// It wraps ErrInvalid so callers classifying ErrInvalid still match it,
	// while errors.Is(err, ErrOldFormat) distinguishes the refusal. M4 does not
	// silently upgrade V1 snapshots.
	ErrOldFormat = fmt.Errorf("%w: obsolete V1 snapshot format; back up and re-provision, or run an explicit migration", ErrInvalid)
)

// ValidName reports whether name matches the account/group grammar
// [A-Za-z0-9_-]{1,31}. The character set matches remotepolicy.ValidUser; the
// 31-byte ceiling is MaxName.
func ValidName(name string) bool {
	return validNameBytes([]byte(name))
}

func validNameBytes(name []byte) bool {
	if len(name) == 0 || len(name) > MaxName {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// reservedUnix reports whether n is a reserved Unix number (0 or 0xFFFFFFFF).
// Both UID and GID allocation and assignment reject these.
func reservedUnix(n uint32) bool {
	return n == ReservedUID || n == ReservedGID
}

func errInvalid(msg string) error   { return fmt.Errorf("%s: %w", msg, ErrInvalid) }
func errNotFound(msg string) error  { return fmt.Errorf("%s: %w", msg, ErrNotFound) }
func errExists(msg string) error    { return fmt.Errorf("%s: %w", msg, ErrExists) }
func errExhausted(msg string) error { return fmt.Errorf("%s: %w", msg, ErrExhausted) }
func errConflict(msg string) error  { return fmt.Errorf("%s: %w", msg, ErrConflict) }

// validate checks the full store invariants: bounds, name grammar, canonical
// ordering, duplicate names/UIDs/GIDs, total-key bound, and dangling group
// references. Decode calls it after parsing; mutation methods maintain the
// invariants directly.
func (s *Store) validate() error {
	if len(s.accounts) > MaxAccounts || len(s.groups) > MaxGroups {
		return ErrInvalid
	}
	if s.nextAccountID == 0 || s.nextGroupID == 0 {
		return ErrInvalid
	}
	// High-water marks are uint64 values bounded by the uint32 domain:
	// [AllocBase, 0xFFFFFFFF], with 0xFFFFFFFF the exhausted sentinel.
	if s.nextUID < uint64(AllocBase) || s.nextUID > uint64(maxAllocatableUnix)+1 {
		return ErrInvalid
	}
	if s.nextGID < uint64(AllocBase) || s.nextGID > uint64(maxAllocatableUnix)+1 {
		return ErrInvalid
	}
	for i := range s.accounts {
		a := &s.accounts[i]
		if i > 0 && s.accounts[i-1].ID >= a.ID {
			return ErrInvalid
		}
		if a.NameLen == 0 || a.NameLen > MaxName || !validNameBytes(a.Name[:a.NameLen]) {
			return ErrInvalid
		}
		if reservedUnix(a.UID) {
			return ErrInvalid
		}
		if a.HomeLen > MaxHome {
			return ErrInvalid
		}
		if a.GroupCount > MaxSupplementaryGroups {
			return ErrInvalid
		}
		if a.KeyCount > MaxKeysPerAccount {
			return ErrInvalid
		}
		for j := 1; j < int(a.GroupCount); j++ {
			if a.Groups[j-1] >= a.Groups[j] {
				return ErrInvalid
			}
		}
		for j := 1; j < int(a.KeyCount); j++ {
			if !keyLess(a.Keys[j-1], a.Keys[j]) {
				return ErrInvalid
			}
		}
	}
	for i := range s.groups {
		g := &s.groups[i]
		if i > 0 && s.groups[i-1].ID >= g.ID {
			return ErrInvalid
		}
		if g.NameLen == 0 || g.NameLen > MaxName || !validNameBytes(g.Name[:g.NameLen]) {
			return ErrInvalid
		}
		if reservedUnix(g.GID) {
			return ErrInvalid
		}
	}
	for i := range s.accounts {
		for j := i + 1; j < len(s.accounts); j++ {
			if s.accounts[i].NameString() == s.accounts[j].NameString() {
				return ErrInvalid
			}
		}
	}
	for i := range s.groups {
		for j := i + 1; j < len(s.groups); j++ {
			if s.groups[i].NameString() == s.groups[j].NameString() {
				return ErrInvalid
			}
		}
	}
	for i := range s.accounts {
		for j := i + 1; j < len(s.accounts); j++ {
			if s.accounts[i].UID == s.accounts[j].UID {
				return ErrInvalid
			}
		}
	}
	for i := range s.groups {
		for j := i + 1; j < len(s.groups); j++ {
			if s.groups[i].GID == s.groups[j].GID {
				return ErrInvalid
			}
		}
	}
	total := 0
	for i := range s.accounts {
		total += int(s.accounts[i].KeyCount)
	}
	if total > MaxTotalKeys {
		return ErrInvalid
	}
	for i := range s.accounts {
		a := &s.accounts[i]
		if a.ID >= s.nextAccountID {
			return ErrInvalid
		}
		if !s.groupIDExists(a.PrimaryGID) {
			return ErrInvalid
		}
		for j := 0; j < int(a.GroupCount); j++ {
			if !s.groupIDExists(a.Groups[j]) {
				return ErrInvalid
			}
		}
	}
	for i := range s.groups {
		if s.groups[i].ID >= s.nextGroupID {
			return ErrInvalid
		}
	}
	// Every issued numeric UID/GID must sit strictly below its high-water mark;
	// otherwise a later automatic allocation could reuse an issued number.
	for i := range s.accounts {
		if uint64(s.accounts[i].UID) >= s.nextUID {
			return ErrInvalid
		}
	}
	for i := range s.groups {
		if uint64(s.groups[i].GID) >= s.nextGID {
			return ErrInvalid
		}
	}
	return nil
}
