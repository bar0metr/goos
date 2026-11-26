package vfs

// SessionIdentity is the trusted immutable principal a session carries, kept
// separate from Authority and from native handles (ADR 0234). PID 1 installs
// it over the trusted control path when it opens a session; ordinary requests
// cannot set or override it. Identity is data, never authority: the UID and
// GID grant nothing by themselves.
//
// A zero SessionIdentity is the identity-free system session: creation keeps
// the legacy fixed ownership (UID 0, GID 0) and the caller's documented
// default mode. The account's supplementary groups, home and name are carried
// elsewhere — the root confinement, environment and Linux credential snapshot
// — not stored in the namespace.
type SessionIdentity struct {
	// AccountID is the stable account ID from the identity database. Zero
	// means no account backs the session.
	AccountID uint64
	// UID is the account's Unix user number.
	UID uint32
	// PrimaryGID is the Unix group number of the account's primary group.
	PrimaryGID uint32
	// Epoch is the 128-bit identity database epoch this snapshot was read
	// from. Together with AccountID it names one stable identity reference.
	Epoch [16]byte
}

// Valid reports whether the identity is well formed. A zero identity is valid
// and means an identity-free system session.
func (identity SessionIdentity) Valid() bool {
	if identity.AccountID == 0 {
		return identity.UID == 0 && identity.PrimaryGID == 0 && identity.Epoch == [16]byte{}
	}
	return identity.UID != 0 && identity.PrimaryGID != 0 && identity.Epoch != [16]byte{}
}

// ownerIdentity keeps a session's identity. Its zero value is an identity-free
// session.
type ownerIdentity struct {
	owner    Owner
	identity SessionIdentity
}

// SetIdentity records the trusted principal of a session. It fails closed on a
// malformed identity or an exhausted identity table; it does not close files.
func (namespace *Namespace) SetIdentity(owner Owner, identity SessionIdentity) Status {
	if owner == 0 || !identity.Valid() {
		return StatusInvalidArgument
	}
	for index := range namespace.identities {
		entry := &namespace.identities[index]
		if entry.owner == owner {
			entry.identity = identity
			return StatusOK
		}
	}
	for index := range namespace.identities {
		entry := &namespace.identities[index]
		if entry.owner == 0 {
			*entry = ownerIdentity{owner: owner, identity: identity}
			return StatusOK
		}
	}
	return StatusResourceExhausted
}

// ForgetIdentity drops a session's identity when the session goes.
func (namespace *Namespace) ForgetIdentity(owner Owner) {
	for index := range namespace.identities {
		if namespace.identities[index].owner == owner {
			namespace.identities[index] = ownerIdentity{}
		}
	}
}

// Identity returns the session's principal, or the zero identity for an
// identity-free system session.
func (namespace *Namespace) Identity(owner Owner) SessionIdentity {
	for index := range namespace.identities {
		if namespace.identities[index].owner == owner {
			return namespace.identities[index].identity
		}
	}
	return SessionIdentity{}
}
